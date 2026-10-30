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
	// Cron is five-field POSIX cron, read in TimeZone (IANA; empty is UTC).
	Cron     string `json:"cron"`
	TimeZone string `json:"time_zone,omitempty"`
	// Input is what each run is asked.
	Input string `json:"input"`
	// Enabled is whether it fires.
	Enabled bool `json:"enabled"`
	// Missed is what a firing that comes late does (MissedWithinHour, the
	// default when empty, or MissedSkip); Busy, one that comes while the
	// schedule's last run is still at work (BusySkip, the default and only
	// rule).
	Missed string `json:"missed,omitempty"`
	Busy   string `json:"busy,omitempty"`
}

// A schedule's rules for a firing that comes late, or while its last run is
// still at work.
const (
	// MissedWithinHour runs a firing missed by less than MissedWithin, once;
	// an older one is skipped.
	MissedWithinHour = "within_1h"
	// MissedSkip skips every missed firing.
	MissedSkip = "skip"
	// BusySkip skips a firing that comes while the schedule's last run is
	// still at work (PENDING or RUNNING; one paused for its owner isn't).
	BusySkip = "skip"
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
	case s.Busy != "" && s.Busy != BusySkip:
		return fmt.Errorf("schedule: busy is %q", BusySkip)
	}
	c, err := ParseCron(s.Cron)
	if err != nil {
		return err
	}
	if c.MinGap() < MinGap {
		return fmt.Errorf("schedule: a schedule fires at most every %d minutes", int(MinGap.Minutes()))
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

// names reports whether the schedule, as stored, fires at due.
func (s Schedule) names(due time.Time) bool {
	c, err1 := ParseCron(s.Cron)
	loc, err2 := s.Location()
	return s.Enabled && err1 == nil && err2 == nil && c.Matches(due.In(loc))
}

// Next is the schedule's first firing after t, or the zero time when it is
// off or never fires. It is for showing; the Scheduler decides the firings.
func (s Schedule) Next(t time.Time) time.Time {
	c, err1 := ParseCron(s.Cron)
	loc, err2 := s.Location()
	if !s.Enabled || err1 != nil || err2 != nil {
		return time.Time{}
	}
	return c.Next(t, loc)
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
)

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
	// Start starts a background run for the firing, once per Key.
	Start func(ctx context.Context, f Firing) error
	// Busy reports whether the schedule's last run, other than the firing's
	// own (a repeated delivery), is still at work: BusySkip skips the
	// firing then. Nil: never busy.
	Busy func(ctx context.Context, f Firing) (bool, error)
	// History, when set, keeps each firing: run, or skipped and why.
	History History
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
	if !s.names(due) {
		return sv.Scheduler.Set(ctx, s)
	}
	f := Firing{Tenant: tenant, Schedule: id, Agent: s.Agent, Input: s.Input, Due: due}
	now := time.Now()
	if sv.Now != nil {
		now = sv.Now()
	}
	// Missed: later than a Scheduler's own delay, and past what the rule runs.
	if late := now.Sub(due); late > MissedAfter && (s.MissedRule() == MissedSkip || late > MissedWithin) {
		return sv.keep(ctx, f, Fired{Due: due, At: now, Outcome: FiredMissed})
	}
	if sv.Busy != nil {
		busy, err := sv.Busy(ctx, f)
		if err != nil {
			return err
		}
		if busy {
			return sv.keep(ctx, f, Fired{Due: due, At: now, Outcome: FiredBusy})
		}
	}
	if err := sv.Start(ctx, f); err != nil {
		return err
	}
	return sv.keep(ctx, f, Fired{Due: due, At: now, Outcome: FiredRun, Run: f.Key()})
}

// keep adds fired to the schedule's history, when there is one.
func (sv *Service) keep(ctx context.Context, f Firing, fired Fired) error {
	if sv.History == nil {
		return nil
	}
	fired.Due, fired.At = fired.Due.UTC(), fired.At.UTC()
	return sv.History.Add(ctx, f.Tenant, f.Schedule, fired)
}
