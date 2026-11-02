// Package schedule is when an agent runs by itself: a cron in a time zone,
// the input each run is asked, on or off. Each firing starts an ordinary
// background run; a run that pauses on a KIFF hold counts as paused, not
// failed.
//
// A schedule is stored (port Schedules) and set up to fire (port Scheduler,
// EventBridge Scheduler in production). Service keeps the two in step: what is
// stored is the truth, and a firing reads the schedule as it is now, so a
// late or repeated firing never runs what was changed or removed since.
package schedule

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata" // zones load the same on any host, a bare Lambda image included
)

// MinGap is how often a schedule may fire at most, as in tafs.
const MinGap = 15 * time.Minute

// MaxInput bounds the input each run is asked, in bytes.
const MaxInput = 32_000

const (
	maxID     = 128
	maxTenant = 256
)

// Schedule is when an agent runs by itself.
type Schedule struct {
	Tenant string `json:"tenant"`
	ID     string `json:"id"`
	// Agent is the agent each firing runs.
	Agent string `json:"agent"`
	// Kind is how it fires: KindCron (the default when empty), KindOnce or
	// KindMonthlyLastDay.
	Kind string `json:"kind,omitempty"`
	// Cron is five-field POSIX cron, read in TimeZone (IANA; empty is UTC).
	// KindMonthlyLastDay keeps its time of day here, as a daily cron
	// ("M H * * *"); KindOnce has none.
	Cron     string `json:"cron,omitempty"`
	TimeZone string `json:"time_zone,omitempty"`
	// At is when a KindOnce schedule fires, to the minute.
	At time.Time `json:"at,omitzero"`
	// FollowUp marks a once schedule the agent set itself,
	// within the bounds Service.FollowUp keeps, and Version the agent's
	// version it was set under: a firing under another version starts
	// nothing.
	FollowUp bool `json:"follow_up,omitempty"`
	Version  int  `json:"version,omitempty"`
	// Input is what each run is asked.
	Input string `json:"input"`
	// Enabled is whether it fires.
	Enabled bool `json:"enabled"`
	// Missed is what a firing that comes late does (MissedWithinHour, the
	// default when empty, or MissedSkip); Busy, one that comes while the
	// agent is still at work, whatever started it (BusySkip, the default,
	// or BusyLatest).
	Missed string `json:"missed,omitempty"`
	Busy   string `json:"busy,omitempty"`
}

// How a schedule fires. The plain forms the dial shows (daily,
// weekdays, chosen days, several times a day, every N minutes between set
// hours) are KindCron, as cron says them exactly.
const (
	KindCron = "cron"
	// KindOnce fires once, at At, and is gone after it fires.
	KindOnce = "once"
	// KindMonthlyLastDay fires on the month's last day at its Cron's time:
	// cron can't say "the last day", so the Scheduler fires it daily and
	// only the last day's firing runs.
	KindMonthlyLastDay = "monthly_last_day"
)

// KindOf is the schedule's Kind, its default filled in.
func (s Schedule) KindOf() string { return cmp.Or(s.Kind, KindCron) }

// A schedule's rules for a firing that comes late, or while its last run is
// still at work.
const (
	// MissedWithinHour runs a firing missed by less than MissedWithin, once;
	// an older one is skipped.
	MissedWithinHour = "within_1h"
	// MissedSkip skips every missed firing.
	MissedSkip = "skip"
	// BusySkip skips a firing that comes while the agent is still at work
	// (a run PENDING or RUNNING; one paused for its owner isn't).
	BusySkip = "skip"
	// BusyLatest keeps it waiting instead, as the agent's one waiting
	// firing: a later one, from any of its schedules, replaces it. It runs
	// once the agent is free (Service.Free), so a burst isn't lost and two
	// firings never run the agent side by side.
	BusyLatest = "latest"
)

// MissedAfter is how late a firing may come and still be on time: a
// Scheduler's own delay. Later than that, it was missed.
const MissedAfter = 5 * time.Minute

// MissedWithin is how late a missed firing may be and still run, under
// MissedWithinHour.
const MissedWithin = time.Hour

// MissedRule is the schedule's Missed, its default filled in.
func (s Schedule) MissedRule() string { return cmp.Or(s.Missed, MissedWithinHour) }

// Check refuses a schedule that can't be stored or fired as written.
func (s Schedule) Check() error {
	switch {
	case s.Tenant == "" || len(s.Tenant) > maxTenant:
		return fmt.Errorf("schedule: a tenant is 1 to %d bytes", maxTenant)
	case !validID(s.ID) || !validID(s.Agent):
		return fmt.Errorf("schedule: an id or agent is 1 to %d letters, digits, '-', '_' or '.'", maxID)
	case s.Input == "" || len(s.Input) > MaxInput:
		return fmt.Errorf("schedule: the input is 1 to %d bytes", MaxInput)
	case s.Missed != "" && s.Missed != MissedWithinHour && s.Missed != MissedSkip:
		return fmt.Errorf("schedule: missed is %q or %q", MissedWithinHour, MissedSkip)
	case s.Busy != "" && s.Busy != BusySkip && s.Busy != BusyLatest:
		return fmt.Errorf("schedule: busy is %q or %q", BusySkip, BusyLatest)
	}
	switch s.KindOf() {
	case KindCron:
		c, err := ParseCron(s.Cron)
		if err != nil {
			return err
		}
		if c.MinGap() < MinGap {
			return fmt.Errorf("schedule: a schedule fires at most every %d minutes", int(MinGap.Minutes()))
		}
		if !s.At.IsZero() {
			return errors.New("schedule: only a once schedule has a time to fire at")
		}
	case KindMonthlyLastDay:
		if !daily(s.Cron) {
			return errors.New("schedule: a monthly last-day schedule keeps its time as a daily cron (\"M H * * *\")")
		}
		if !s.At.IsZero() {
			return errors.New("schedule: only a once schedule has a time to fire at")
		}
	case KindOnce:
		if s.Cron != "" || s.At.IsZero() || !s.At.Equal(s.At.Truncate(time.Minute)) {
			return errors.New("schedule: a once schedule has a time to fire at, to the minute, and no cron")
		}
	default:
		return fmt.Errorf("schedule: kind is %q, %q or %q", KindCron, KindOnce, KindMonthlyLastDay)
	}
	if s.FollowUp && s.KindOf() != KindOnce {
		return errors.New("schedule: a follow-up fires once")
	}
	if _, err := s.Location(); err != nil {
		return err
	}
	return nil
}

// Location is the schedule's time zone.
func (s Schedule) Location() (*time.Location, error) {
	if s.TimeZone == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(s.TimeZone)
	if err != nil || s.TimeZone == "Local" {
		return nil, fmt.Errorf("schedule: %q is not an IANA time zone", s.TimeZone)
	}
	return loc, nil
}

// daily reports whether expr is a cron that fires once a day, every day.
func daily(expr string) bool {
	f := strings.Fields(expr)
	if len(f) != 5 || f[2] != "*" || f[3] != "*" || f[4] != "*" {
		return false
	}
	c, err := ParseCron(expr)
	return err == nil && len(values(c.minute)) == 1 && len(values(c.hour)) == 1
}

// lastDay reports whether t is the last day of its month, on t's own clock.
func lastDay(t time.Time) bool { return t.AddDate(0, 0, 1).Day() == 1 }

// names reports whether the schedule, as stored, fires at due.
func (s Schedule) names(due time.Time) bool {
	if !s.Enabled {
		return false
	}
	if s.KindOf() == KindOnce {
		return due.Equal(s.At)
	}
	c, err1 := ParseCron(s.Cron)
	loc, err2 := s.Location()
	if err1 != nil || err2 != nil || !c.Matches(due.In(loc)) {
		return false
	}
	return s.KindOf() != KindMonthlyLastDay || lastDay(due.In(loc))
}

// Next is the schedule's first firing after t, or the zero time when it is
// off or never fires. It is for showing; the Scheduler decides the firings.
func (s Schedule) Next(t time.Time) time.Time {
	if !s.Enabled {
		return time.Time{}
	}
	if s.KindOf() == KindOnce {
		if s.At.After(t) {
			return s.At
		}
		return time.Time{}
	}
	c, err1 := ParseCron(s.Cron)
	loc, err2 := s.Location()
	if err1 != nil || err2 != nil {
		return time.Time{}
	}
	next := c.Next(t, loc)
	for s.KindOf() == KindMonthlyLastDay && !next.IsZero() && !lastDay(next) {
		next = c.Next(next, loc)
	}
	return next
}

func validID(id string) bool {
	if id == "" || len(id) > maxID {
		return false
	}
	for _, c := range []byte(id) {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// A follow-up's bounds. The server keeps them, never the
// model's words.
const (
	// MaxFollowUps is how many follow-ups an agent may have waiting to fire.
	MaxFollowUps = 3
	// FollowUpWithin is how far ahead a follow-up may be set: one that
	// hasn't fired by then never does.
	FollowUpWithin = 7 * 24 * time.Hour
)

// Why a follow-up is refused.
var (
	ErrFollowUpChain = errors.New("schedule: a follow-up's run can't set another follow-up")
	ErrFollowUpLimit = fmt.Errorf("schedule: the agent has %d follow-ups waiting, the most it may", MaxFollowUps)
	ErrFollowUpRange = fmt.Errorf("schedule: a follow-up fires in the future, within %d days", int(FollowUpWithin.Hours()/24))
)

// ErrNotFound means the tenant has no schedule with that ID.
var ErrNotFound = errors.New("schedule: no such schedule")

// ErrNoAgent means the schedule names an agent the tenant doesn't have.
var ErrNoAgent = errors.New("schedule: the tenant has no such agent")

// Schedules stores schedules. Every call is scoped by tenant.
type Schedules interface {
	// Put stores s, replacing the tenant's schedule with its ID.
	Put(ctx context.Context, s Schedule) error
	// Get returns the tenant's schedule, or ErrNotFound.
	Get(ctx context.Context, tenant, id string) (Schedule, error)
	// List returns the tenant's schedules, by ID.
	List(ctx context.Context, tenant string) ([]Schedule, error)
	// Delete removes the tenant's schedule; one that isn't there is not an
	// error.
	Delete(ctx context.Context, tenant, id string) error
}

// Fired is one firing as a schedule's history keeps it: when it was due, when
// it came, and what came of it: its run, or why it was skipped.
type Fired struct {
	Due     time.Time `json:"due"`
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome"`
	// Run is the run it started (Outcome FiredRun).
	Run string `json:"run,omitempty"`
}

// What came of a firing.
const (
	FiredRun    = "run"
	FiredMissed = "skipped_missed"
	FiredBusy   = "skipped_busy"
	// FiredWaiting waits for the agent to be free (BusyLatest); it becomes
	// FiredRun when it runs, or FiredReplaced, FiredDropped.
	FiredWaiting  = "waiting"
	FiredReplaced = "skipped_replaced" // a later firing took its place
	FiredDropped  = "skipped_dropped"  // its schedule changed or went before the agent was free
)

// Waiting is the agent's one firing waiting for it to be free.
type Waiting struct {
	Tenant   string    `json:"tenant"`
	Agent    string    `json:"agent"`
	Schedule string    `json:"schedule"`
	Due      time.Time `json:"due"`
}

// Waitings keeps each agent's one waiting firing.
type Waitings interface {
	// Swap makes w its agent's waiting firing, and returns the one it
	// replaced, if any.
	Swap(ctx context.Context, w Waiting) (prev Waiting, replaced bool, err error)
	// Take removes the agent's waiting firing and returns it, if any.
	Take(ctx context.Context, tenant, agent string) (Waiting, bool, error)
	// List returns up to limit waiting firings, across tenants, for the
	// sweep that frees agents whose runs ended without a word.
	List(ctx context.Context, limit int) ([]Waiting, error)
}

// MaxHistory is how many firings a schedule's history keeps, newest first.
const MaxHistory = 100

// History keeps each schedule's firings. Every call is scoped by tenant. A
// firing is kept once by its schedule and due time: the same firing kept
// again replaces it.
type History interface {
	// Add keeps f in the schedule's history.
	Add(ctx context.Context, tenant, id string, f Fired) error
	// Firings returns the schedule's history, newest first, at most limit
	// (at most MaxHistory).
	Firings(ctx context.Context, tenant, id string, limit int) ([]Fired, error)
}

// Scheduler fires schedules: EventBridge Scheduler in production. Each
// firing calls Service.Fire with the schedule's tenant and ID and the time
// it was due. A tenant and an ID can be longer than a name the Scheduler
// takes (EventBridge: 64 characters), so an implementation derives its names
// from both and carries them in what each firing sends.
type Scheduler interface {
	// Set makes s fire as it says, or stop firing when it is off, replacing
	// what was set for it before. s has passed Check.
	Set(ctx context.Context, s Schedule) error
	// Remove stops the schedule for good. Removing one that isn't set is not
	// an error.
	Remove(ctx context.Context, tenant, id string) error
}

// Firing is one firing of a schedule: the run it starts.
type Firing struct {
	Tenant   string
	Schedule string
	Agent    string
	Input    string
	// Due is when the firing was due.
	Due time.Time
}

// Key is the same for every delivery of one firing. A Scheduler may deliver
// a firing more than once (EventBridge Scheduler delivers at least once), so
// Start uses Key to start one run per firing, however often it comes.
func (f Firing) Key() string {
	h := sha256.Sum256(fmt.Appendf(nil, "%d:%s|%d:%s|%d", len(f.Tenant), f.Tenant, len(f.Schedule), f.Schedule, f.Due.UTC().Unix()))
	return hex.EncodeToString(h[:16])
}

// Service keeps the stored schedules and the Scheduler in step, and starts a
// run for each firing.
type Service struct {
	Store     Schedules
	Scheduler Scheduler
	// Live reports whether the tenant has the agent. Fire deletes the
	// schedules of an agent Live says is gone, so Live is a consistent read
	// of "the agent exists": never "is enabled", never a stale read.
	Live func(ctx context.Context, tenant, agent string) (bool, error)
	// Version, when set, is the agent's live version: a follow-up set under
	// another starts nothing, and is dropped.
	Version func(ctx context.Context, tenant, agent string) (int, error)
	// Start starts a background run for the firing, once per Key.
	Start func(ctx context.Context, f Firing) error
	// Busy reports whether the schedule's last run, other than the firing's
	// own (a repeated delivery), is still at work: BusySkip skips the
	// firing then. Nil: never busy.
	Busy func(ctx context.Context, f Firing) (bool, error)
	// History, when set, keeps each firing: run, or skipped and why.
	History History
	// Waitings keeps the firings BusyLatest holds; without it, BusyLatest
	// skips as BusySkip does.
	Waitings Waitings
	// Now is the clock a firing's lateness is read by; nil is time.Now.
	Now func() time.Time
}

// Put stores s and sets it to fire. Its agent must exist: schedules are
// made only for live agents. If setting it fails, the schedule is stored but
// may not fire as stored: the caller retries Put, which is safe to repeat.
func (sv *Service) Put(ctx context.Context, s Schedule) error {
	if err := s.Check(); err != nil {
		return err
	}
	live, err := sv.Live(ctx, s.Tenant, s.Agent)
	if err != nil {
		return err
	}
	if !live {
		return ErrNoAgent
	}
	if err := sv.Store.Put(ctx, s); err != nil {
		return err
	}
	return sv.Scheduler.Set(ctx, s)
}

// FollowUp stores and sets s as a follow-up its agent set itself, from a
// run: once, in the future and within FollowUpWithin, at most MaxFollowUps
// waiting per agent. fromFollowUp is whether that run was itself started by
// a follow-up: a chain could become recurring work nobody confirmed, so it
// is refused.
func (sv *Service) FollowUp(ctx context.Context, s Schedule, fromFollowUp bool) error {
	if fromFollowUp {
		return ErrFollowUpChain
	}
	s.Kind, s.FollowUp, s.Enabled = KindOnce, true, true
	if now := sv.now(); !s.At.After(now) || s.At.After(now.Add(FollowUpWithin)) {
		return ErrFollowUpRange
	}
	all, err := sv.Store.List(ctx, s.Tenant)
	if err != nil {
		return err
	}
	open := 0
	for _, o := range all {
		if o.FollowUp && o.Enabled && o.Agent == s.Agent && o.ID != s.ID {
			open++
		}
	}
	if open >= MaxFollowUps {
		return ErrFollowUpLimit
	}
	return sv.Put(ctx, s)
}

// CancelFollowUps deletes the agent's follow-ups that wait to fire: its
// version switched, it was paused or halted, or its Card was revoked. Safe
// to repeat.
func (sv *Service) CancelFollowUps(ctx context.Context, tenant, agent string) error {
	all, err := sv.Store.List(ctx, tenant)
	if err != nil {
		return err
	}
	for _, s := range all {
		if s.FollowUp && s.Agent == agent {
			if err := sv.Delete(ctx, tenant, s.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// Delete stops the schedule, then forgets it. Safe to repeat.
func (sv *Service) Delete(ctx context.Context, tenant, id string) error {
	if err := sv.Scheduler.Remove(ctx, tenant, id); err != nil {
		return err
	}
	return sv.Store.Delete(ctx, tenant, id)
}

// DeleteAgent deletes every schedule of the tenant's agent, when the agent is
// removed. Safe to repeat.
func (sv *Service) DeleteAgent(ctx context.Context, tenant, agent string) error {
	all, err := sv.Store.List(ctx, tenant)
	if err != nil {
		return err
	}
	for _, s := range all {
		if s.Agent != agent {
			continue
		}
		if err := sv.Delete(ctx, tenant, s.ID); err != nil {
			return err
		}
	}
	return nil
}

// Fire starts the run for one firing of the tenant's schedule, due at due.
// It reads the schedule as it is now, and what is stored is the truth: when
// the Scheduler disagrees with it (a Set that failed, two Puts that raced),
// the firing starts nothing and the Scheduler is put back in line.
//
//   - A schedule no longer stored is removed from the Scheduler.
//   - A schedule whose agent is gone is deleted.
//   - A firing the stored schedule doesn't name (it is off, or its cron
//     doesn't fire then) sets the stored schedule again.
//
// Starting nothing is not an error. A failure to put the Scheduler back in
// line is, so whatever retries the invocation tries again; failing that, the
// next mismatched firing does.
func (sv *Service) Fire(ctx context.Context, tenant, id string, due time.Time) error {
	s, err := sv.Store.Get(ctx, tenant, id)
	if errors.Is(err, ErrNotFound) {
		return sv.Scheduler.Remove(ctx, tenant, id)
	}
	if err != nil {
		return err
	}
	live, err := sv.Live(ctx, tenant, s.Agent)
	if err != nil {
		return err
	}
	if !live {
		return sv.Delete(ctx, tenant, id)
	}
	if s.KindOf() == KindOnce && !s.Enabled {
		return sv.Scheduler.Remove(ctx, tenant, id) // fired, or off: nothing to set again
	}
	if !s.names(due) {
		return sv.Scheduler.Set(ctx, s)
	}
	f := Firing{Tenant: tenant, Schedule: id, Agent: s.Agent, Input: s.Input, Due: due}
	now := sv.now()
	if s.FollowUp && sv.Version != nil {
		v, err := sv.Version(ctx, tenant, s.Agent)
		if err != nil {
			return err
		}
		if v != s.Version {
			return sv.keep(ctx, s, f, Fired{Due: due, At: now, Outcome: FiredDropped})
		}
	}
	// Missed: later than a Scheduler's own delay, and past what the rule runs.
	if late := now.Sub(due); late > MissedAfter && (s.MissedRule() == MissedSkip || late > MissedWithin) {
		return sv.keep(ctx, s, f, Fired{Due: due, At: now, Outcome: FiredMissed})
	}
	if sv.Busy != nil {
		busy, err := sv.Busy(ctx, f)
		if err != nil {
			return err
		}
		if busy && s.Busy == BusyLatest && sv.Waitings != nil {
			prev, replaced, err := sv.Waitings.Swap(ctx, Waiting{Tenant: tenant, Agent: s.Agent, Schedule: id, Due: due})
			if err != nil {
				return err
			}
			if replaced && !(prev.Schedule == id && prev.Due.Equal(due)) { // a repeated delivery replaces itself
				if err := sv.keepOf(ctx, prev, FiredReplaced, now); err != nil {
					return err
				}
			}
			return sv.keepAs(ctx, f, Fired{Due: due, At: now, Outcome: FiredWaiting})
		}
		if busy {
			return sv.keep(ctx, s, f, Fired{Due: due, At: now, Outcome: FiredBusy})
		}
	}
	if err := sv.Start(ctx, f); err != nil {
		return err
	}
	return sv.keep(ctx, s, f, Fired{Due: due, At: now, Outcome: FiredRun, Run: f.Key()})
}

// Free runs the agent's waiting firing, if any, now that its run has ended.
// The firing is read against its schedule as it is now: a schedule changed
// so it no longer names that firing, switched off, removed, or no longer
// BusyLatest drops it, and so does an agent gone. An agent busy again keeps
// it waiting. Safe to call for any agent, at any time.
func (sv *Service) Free(ctx context.Context, tenant, agent string) error {
	if sv.Waitings == nil {
		return nil
	}
	w, ok, err := sv.Waitings.Take(ctx, tenant, agent)
	if err != nil || !ok {
		return err
	}
	now := sv.now()
	s, err := sv.Store.Get(ctx, tenant, w.Schedule)
	if errors.Is(err, ErrNotFound) {
		return nil // gone, with its history
	}
	if err != nil {
		return sv.putBack(ctx, w, err)
	}
	if s.Agent != agent || s.Busy != BusyLatest || !s.names(w.Due) {
		return sv.keepOf(ctx, w, FiredDropped, now)
	}
	live, err := sv.Live(ctx, tenant, agent)
	if err != nil {
		return sv.putBack(ctx, w, err)
	}
	if !live {
		return sv.Delete(ctx, tenant, w.Schedule)
	}
	f := Firing{Tenant: tenant, Schedule: w.Schedule, Agent: agent, Input: s.Input, Due: w.Due}
	if sv.Busy != nil {
		busy, err := sv.Busy(ctx, f)
		if err != nil {
			return sv.putBack(ctx, w, err)
		}
		if busy { // another run began meanwhile: it waits for that one
			return sv.putBack(ctx, w, nil)
		}
	}
	if err := sv.Start(ctx, f); err != nil {
		return sv.putBack(ctx, w, err)
	}
	return sv.keep(ctx, s, f, Fired{Due: w.Due, At: now, Outcome: FiredRun, Run: f.Key()})
}

// FreeAll frees up to limit agents with a waiting firing, for the sweep: a
// run can end where nobody calls Free (an HTTP chat, the sweep itself).
func (sv *Service) FreeAll(ctx context.Context, limit int) error {
	if sv.Waitings == nil {
		return nil
	}
	all, err := sv.Waitings.List(ctx, limit)
	if err != nil {
		return err
	}
	var errs []error
	for _, w := range all {
		errs = append(errs, sv.Free(ctx, w.Tenant, w.Agent))
	}
	return errors.Join(errs...)
}

// putBack makes w its agent's waiting firing again, unless a later one took
// the place meanwhile, and returns cause.
func (sv *Service) putBack(ctx context.Context, w Waiting, cause error) error {
	prev, replaced, err := sv.Waitings.Swap(ctx, w)
	if err != nil {
		return errors.Join(cause, err)
	}
	if replaced && prev.Due.After(w.Due) { // the later one stays
		if _, _, err := sv.Waitings.Swap(ctx, prev); err != nil {
			return errors.Join(cause, err)
		}
		return errors.Join(cause, sv.keepOf(ctx, w, FiredReplaced, sv.now()))
	}
	return cause
}

func (sv *Service) now() time.Time {
	if sv.Now != nil {
		return sv.Now()
	}
	return time.Now()
}

// keepOf keeps what came of a waiting firing in its schedule's history.
func (sv *Service) keepOf(ctx context.Context, w Waiting, outcome string, at time.Time) error {
	return sv.keepAs(ctx, Firing{Tenant: w.Tenant, Schedule: w.Schedule}, Fired{Due: w.Due, At: at, Outcome: outcome})
}

// keepAs adds fired to the history, and nothing else.
func (sv *Service) keepAs(ctx context.Context, f Firing, fired Fired) error {
	if sv.History == nil {
		return nil
	}
	fired.Due, fired.At = fired.Due.UTC(), fired.At.UTC()
	return sv.History.Add(ctx, f.Tenant, f.Schedule, fired)
}

// keep adds fired to the schedule's history, when there is one. A once
// schedule is then off for good, whatever came of its firing: the Scheduler
// deleted its own after firing, and the stored one stays, off, so its
// history does too.
func (sv *Service) keep(ctx context.Context, s Schedule, f Firing, fired Fired) error {
	if err := sv.keepAs(ctx, f, fired); err != nil {
		return err
	}
	if s.KindOf() == KindOnce {
		s.Enabled = false
		return sv.Store.Put(ctx, s)
	}
	return nil
}
