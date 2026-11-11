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
	"log/slog"
	"reflect"
	"slices"
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
	// AppCheck, when set, makes each firing a check of a connected app: one
	// read first, and a run only for what is new (Check).
	AppCheck *Check `json:"check,omitempty"`
	// Skip is the coming firings the owner skipped, by their due time: each
	// starts nothing and is kept as skipped by the owner. At most MaxSkips;
	// a skip well past its time is pruned when its schedule next fires.
	Skip []time.Time `json:"skip,omitempty"`
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
	// Failing, kept by the service, is the run of firings whose runs failed
	// before doing any work (FiredUnworked), up to now: unset once a run
	// works. A caller that sets it is ignored.
	Failing *Failing `json:"failing,omitempty"`
	// Waited, kept by the service, is the last firing that started no run
	// because the account's tafs wait for credits (FiredCredits): its due
	// time and why. Unset once one of its runs starts. A caller that sets it
	// is ignored.
	Waited *Waited `json:"waited,omitempty"`
	// Ceiling, kept by the service, is the last day a firing of it was
	// stopped by its agent's daily ceiling (CeilingOf): that day, in the
	// schedule's time zone, and the runs counted. A caller that sets it is
	// ignored.
	Ceiling *Ceiling `json:"ceiling,omitempty"`
}

// Ceiling is a day an agent's scheduled runs reached its ceiling: the day
// (YYYY-MM-DD, in the schedule's time zone) and the runs it had started.
type Ceiling struct {
	Day  string `json:"day"`
	Runs int    `json:"runs"`
}

// An agent's scheduled runs in a day stop at its ceiling: the firings its
// recurring schedules have due that day, plus CeilingFollowUps for the
// follow-ups it sets itself, never above CeilingMax (a run every 15
// minutes, MinGap's most). Past it, a firing starts nothing until the next
// day, so a check that hands everything, or follow-ups that keep coming,
// can't wake the agent without end.
const (
	CeilingFollowUps = 8
	CeilingMax       = 96
)

// Waited is a firing that started no run because the account's tafs wait
// for credits: its due time, and why, KIFF's code (credits_used_up,
// limit_reached or no_plan).
type Waited struct {
	Due time.Time `json:"due"`
	Why string    `json:"why"`
}

// Failing is a schedule's firings in a row whose runs failed before any
// turn, or whose check couldn't read the app: since the first's due time, how
// many, the last one's firing key (its run's ID, when a run was made), and its
// due time. What they were handed waits for a run that works (a check's items go
// back).
type Failing struct {
	Since   time.Time `json:"since"`
	Firings int       `json:"firings"`
	Run     string    `json:"run,omitempty"`
	// Outcome is the last counted firing's outcome (FiredUnworked,
	// FiredCheckFailed, FiredUnfinished), so whoever tells the owner can
	// say which: a run that couldn't start, a check that couldn't read, or
	// a run that stopped partway. Empty on a failing stored before it was
	// kept.
	Outcome string `json:"outcome,omitempty"`
	// Last is the due time of the last firing counted: one no later than it
	// (a repeated delivery, or a late one of an older firing) is counted
	// already. Zero on a failing stored before it was kept.
	Last time.Time `json:"last,omitzero"`
	// Paused: the schedule switched itself off at PauseAfter firings in a
	// row, so whoever tells the owner can say it was the failures, not
	// them. Switching it on again starts its count anew.
	Paused bool `json:"paused,omitempty"`
}

// A schedule that keeps failing backs off on its own: from SlowAfter
// firings in a row that did no work it fires at half its rate, never more
// often than hourly (slowed), and at PauseAfter it switches itself off. One
// run that works, or the owner switching it on again, starts it anew.
// Quiet firings, credit waits and halts are never failures, so they never
// slow it. A once schedule (a follow-up) fires once anyway: never slowed.
const (
	SlowAfter  = 3
	PauseAfter = 6
)

// slowed reports whether the firing due at due is one a failing schedule
// skips: less than twice its step, and less than an hour, after the last
// firing that failed.
func (s Schedule) slowed(due time.Time) bool {
	f := s.Failing
	if f == nil || f.Firings < SlowAfter || f.Last.IsZero() || s.KindOf() == KindOnce {
		return false
	}
	gap := time.Hour
	if next := s.Next(f.Last); !next.IsZero() {
		gap = max(gap, 2*next.Sub(f.Last))
	}
	return due.Sub(f.Last) < gap
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
	if s.AppCheck != nil {
		if s.FollowUp {
			return errors.New("schedule: a follow-up checks nothing first")
		}
		if err := s.AppCheck.check(); err != nil {
			return err
		}
	}
	if len(s.Skip) > MaxSkips {
		return fmt.Errorf("schedule: at most %d coming firings are skipped", MaxSkips)
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
	for !next.IsZero() && (s.KindOf() == KindMonthlyLastDay && !lastDay(next) || slices.ContainsFunc(s.Skip, next.Equal)) {
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
	// ErrNewerWaiting: a firing due later already waits for the agent.
	ErrNewerWaiting  = errors.New("schedule: a later firing already waits for the agent")
	ErrFollowUpLimit = fmt.Errorf("schedule: the agent has %d follow-ups waiting, the most it may", MaxFollowUps)
	ErrFollowUpRange = fmt.Errorf("schedule: a follow-up fires in the future, within %d days", int(FollowUpWithin.Hours()/24))
)

// MaxSkips is how many coming firings a schedule may have skipped.
const MaxSkips = 20

// ErrNotFound means the tenant has no schedule with that ID.
var ErrNotFound = errors.New("schedule: no such schedule")

// ErrUnworked is what Start returns for a firing whose run failed before any
// turn: it did nothing, so nothing it was handed is used up (FiredUnworked).
var ErrUnworked = errors.New("schedule: the firing's run failed before doing any work")

// ErrHalted is what Start, or a check's read, returns for a firing of a
// halted agent: it starts no run and isn't tried again (FiredHalted). A
// check's cursor stays where it was.
var ErrHalted = errors.New("schedule: the agent is halted, so the firing starts nothing")

// ErrSwitching is what Start returns for a firing of an agent switching
// versions: it starts no run and isn't tried again, since a retry could start
// it on the version the owner is leaving, or late on the next one
// (kiff/tafs#107). Nothing failed, and nothing was used up: a check's cursor
// stays, and the next firing hands the same items to the new version
// (FiredSwitching).
var ErrSwitching = errors.New("schedule: the agent is switching versions, so the firing starts nothing")

// ErrUnfinished is what Start returns for a firing whose run did some work,
// then failed or was cut short by its turn or cost cap, with no call that
// could have changed anything outside it. What a check handed it goes to the
// next firing's run again (FiredRetrying), MaxCheckTries runs at most.
var ErrUnfinished = errors.New("schedule: the firing's run didn't finish, and changed nothing")

// ErrNotAgain is what Start returns for a firing whose run didn't finish
// either, and whose items must not go to another run: a call of it that acts
// may have reached the vendor, so a second run could pay or refund again, or
// a turn of it was declined, so a second run would only be declined too
// (FiredUnfinished).
var ErrNotAgain = errors.New("schedule: the firing's run didn't finish, and what it was handed isn't handed again")

// CreditsError is what Start returns for a firing whose run KIFF's balance
// refused, the account's tafs waiting for credits: Why is KIFF's code. It
// starts no run. What the firing was handed waits for the next firing that
// runs, as after a run that did nothing, but it isn't a failure: the
// schedule's failing doesn't move (FiredCredits).
type CreditsError struct{ Why string }

func (e *CreditsError) Error() string {
	return "schedule: the account's tafs wait for credits (" + e.Why + "), so the firing starts nothing"
}

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
	// PutFollowUp stores s, a follow-up its agent set, only while the agent
	// has fewer than max other enabled follow-ups: counted and stored in one
	// step, so concurrent calls can't pass the limit together.
	// ErrFollowUpLimit otherwise.
	PutFollowUp(ctx context.Context, s Schedule, max int) error
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
	FiredSkipped  = "skipped_by_owner" // the owner skipped this one coming firing
	FiredQuiet    = "quiet"            // its check found nothing new: no run
	// FiredUnworked: its run failed before any turn (the model refused it,
	// say, for want of credits), so it did nothing. What it was handed goes
	// back: a check's cursor stays, and the next firing hands the same items.
	FiredUnworked = "failed_unworked"
	// FiredTooLarge: its check found only items too large for a run's
	// input: no run, and they aren't read again.
	FiredTooLarge = "skipped_too_large"
	// FiredHalted: its agent, or the whole account, was halted. No run, and
	// the firing isn't tried again. A check reads nothing and its cursor
	// stays, so what came meanwhile goes to the first firing after the halt.
	// The schedule's failing is left as it was, since no run worked.
	FiredHalted = "skipped_halted"
	// FiredRetrying: its check's run did some work, then failed or was cut
	// short by its turn or cost cap, and changed nothing (ErrUnfinished).
	// The cursor stays, and the next firing hands the same items to a new
	// run, told to check what was done. The schedule's failing is left as
	// it was.
	FiredRetrying = "failed_retrying"
	// FiredUnfinished: its check's run didn't finish, and what it was
	// handed isn't handed again: a call of it may have changed something or
	// a turn of it was declined (ErrNotAgain), or MaxCheckTries runs were
	// handed them. The cursor moves
	// past them, and it counts toward the schedule's failing, so the owner
	// hears.
	FiredUnfinished = "failed_unfinished"
	// FiredCredits: the account's tafs wait for credits. No run, and what
	// it was handed waits: a check's cursor stays, and the next firing that
	// runs hands the same items. The schedule's failing is left as it was,
	// since nothing failed; its Waited says so.
	FiredCredits = "skipped_credits"
	// FiredCheckFailed: its check couldn't read the app (the tool refused or
	// failed, or answered something that isn't a list). No run, the cursor
	// stays, and it counts toward the schedule's failing like a run that
	// failed before any work, so whoever watches failing hears of it.
	FiredCheckFailed = "failed_check"
	// FiredSwitching: its agent was switching versions. No run, and nothing
	// was used up: a check's cursor stays, so the next firing hands the same
	// items to the new version. The schedule's failing is left as it was,
	// since nothing ran or failed.
	FiredSwitching = "skipped_switching"
	// FiredSlowed: its schedule kept failing (SlowAfter firings in a row),
	// so it fires at half its rate: no run, nothing handed, and it isn't
	// counted as a failure.
	FiredSlowed = "skipped_slowed"
	// FiredCeiling: its agent's scheduled runs reached the day's ceiling
	// (CeilingOf). No run, nothing handed: a check's cursor stays, so the
	// next day's first firing hands the same items. Not a failure.
	FiredCeiling = "skipped_ceiling"
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
	// replaced, if any. One due later stays, compared in the same step, as
	// deliveries come in any order: ErrNewerWaiting, and w doesn't wait.
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
	// Probe makes a check's read through the agent's own Card, without the
	// model, and returns its result. Without it, a schedule with a check
	// starts nothing and is kept as skipped. An error that is a *CheckError
	// says why in its Reason; any other is a failed read.
	Probe func(ctx context.Context, tenant, agent string, c Check) (string, error)
	// Log, when set, gets one line for each check that couldn't be used
	// ("check_failed": the agent, the check's tool and a CheckReason).
	Log *slog.Logger
	// Version, when set, is the agent's live version: a follow-up set under
	// another starts nothing, and is dropped.
	Version func(ctx context.Context, tenant, agent string) (int, error)
	// Start starts a background run for the firing, once per Key. It returns
	// ErrUnworked when that run failed before any turn: the firing then uses
	// up nothing, and is kept as FiredUnworked. It returns ErrHalted when the
	// agent is halted: no run starts, and the firing is kept as FiredHalted.
	// It returns a *CreditsError when the account's tafs wait for credits:
	// no run starts, what the firing was handed waits, and it's kept as
	// FiredCredits.
	Start func(ctx context.Context, f Firing) error
	// RunEnded says how a run started by hand with a check's items
	// (TriggerNow) ended: done false while it hasn't; once it has, err is
	// what Start would have returned for it (ErrUnworked, ErrSwitching,
	// ErrUnfinished, ErrNotAgain or nil). Nil: such a run is never looked
	// at again, and its items aren't handed twice.
	RunEnded func(ctx context.Context, tenant, run string) (done bool, err error)
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
// made only for live agents. One switched from off to on loses its
// failing. If setting it fails, the schedule is stored but
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
	// Switched from off to on, by an update or by its switch, it starts
	// anew: a schedule paused for its failures (PauseAfter) fires at its
	// full rate once the owner resumes it.
	if s.Enabled && s.Failing != nil {
		old, err := sv.Store.Get(ctx, s.Tenant, s.ID)
		switch {
		case err == nil && !old.Enabled:
			s.Failing = nil
		case err != nil && !errors.Is(err, ErrNotFound):
			return err
		}
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
	// The limit is the store's to keep: two runs' calls can come at once.
	if err := sv.Store.PutFollowUp(ctx, s, MaxFollowUps); err != nil {
		return err
	}
	return sv.Scheduler.Set(ctx, s)
}

// OpenFollowUp reports whether o counts toward its agent's follow-ups
// waiting, beside s: an enabled follow-up of the same agent, not s itself.
func OpenFollowUp(o, s Schedule) bool {
	return o.FollowUp && o.Enabled && o.Agent == s.Agent && o.ID != s.ID
}

// CancelFollowUps deletes the agent's follow-ups that wait to fire: its
// version switched, it was paused or halted, or its Card was revoked. Safe
// to repeat. A server run without schedules has none to cancel.
func (sv *Service) CancelFollowUps(ctx context.Context, tenant, agent string) error {
	if sv == nil {
		return nil
	}
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
	if slices.ContainsFunc(s.Skip, due.Equal) {
		// Kept until it is past any late or repeated delivery, then pruned:
		// a skip used up on the first delivery would let a repeat run.
		if kept := slices.DeleteFunc(slices.Clone(s.Skip), func(t time.Time) bool { return t.Before(now.Add(-2 * MissedWithin)) }); len(kept) != len(s.Skip) {
			s.Skip = kept
			if err := sv.Store.Put(ctx, s); err != nil {
				return err
			}
		}
		return sv.keep(ctx, s, f, Fired{Due: due, At: now, Outcome: FiredSkipped})
	}
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
	if s.slowed(due) {
		if sv.Log != nil {
			sv.Log.InfoContext(ctx, "schedule_slowed", "agent", s.Agent, "schedule", id, "failing", s.Failing.Firings)
		}
		return sv.keep(ctx, s, f, Fired{Due: due, At: now, Outcome: FiredSlowed})
	}
	if sv.Busy != nil {
		busy, err := sv.Busy(ctx, f)
		if err != nil {
			return err
		}
		if busy && s.Busy == BusyLatest && sv.Waitings != nil {
			prev, replaced, err := sv.Waitings.Swap(ctx, Waiting{Tenant: tenant, Agent: s.Agent, Schedule: id, Due: due})
			if errors.Is(err, ErrNewerWaiting) { // came late: the later firing waits in its place
				return sv.keep(ctx, s, f, Fired{Due: due, At: now, Outcome: FiredReplaced})
			}
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
	if over, err := sv.overCeiling(ctx, s, due); err != nil {
		return err
	} else if over {
		return sv.keep(ctx, s, f, Fired{Due: due, At: now, Outcome: FiredCeiling})
	}
	return sv.start(ctx, s, f, now)
}

// dayOf is the day due falls on in s's time zone, and its bounds.
func (s Schedule) dayOf(due time.Time) (string, time.Time, time.Time) {
	loc, err := s.Location()
	if err != nil {
		loc = time.UTC
	}
	d := due.In(loc)
	start := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
	return start.Format(time.DateOnly), start, start.AddDate(0, 0, 1)
}

// dueIn counts the firings s has due in [from, to): none for a once
// schedule, which CeilingFollowUps covers.
func (s Schedule) dueIn(from, to time.Time) int {
	if s.KindOf() == KindOnce || !s.Enabled {
		return 0
	}
	n := 0
	for t := s.Next(from.Add(-time.Nanosecond)); !t.IsZero() && t.Before(to) && n <= CeilingMax; t = s.Next(t) {
		if s.names(t) {
			n++
		}
	}
	return n
}

// CeilingOf is an agent's ceiling on the day from..to, from its schedules.
func CeilingOf(schedules []Schedule, from, to time.Time) int {
	n := CeilingFollowUps
	for _, s := range schedules {
		n += s.dueIn(from, to)
	}
	return min(n, CeilingMax)
}

// overCeiling reports whether the firing due at due would start a run past
// its agent's ceiling for that day: the runs its schedules started that
// day, from their history, against CeilingOf. The first firing stopped on a
// day is said once, on the schedule (Ceiling) and in the log. Without a
// History nothing is counted, so nothing is stopped.
func (sv *Service) overCeiling(ctx context.Context, s Schedule, due time.Time) (bool, error) {
	if sv.History == nil {
		return false, nil
	}
	all, err := sv.Store.List(ctx, s.Tenant)
	if err != nil {
		return false, err
	}
	day, from, to := s.dayOf(due)
	mine := slices.DeleteFunc(all, func(o Schedule) bool { return o.Agent != s.Agent })
	runs := 0
	for _, o := range mine {
		h, err := sv.History.Firings(ctx, s.Tenant, o.ID, MaxHistory)
		if err != nil {
			return false, err
		}
		for _, fired := range h {
			if fired.Outcome == FiredRun && !fired.Due.Before(from) && fired.Due.Before(to) {
				runs++
			}
		}
	}
	ceiling := CeilingOf(mine, from, to)
	if runs < ceiling {
		return false, nil
	}
	if s.Ceiling == nil || s.Ceiling.Day != day {
		if sv.Log != nil {
			sv.Log.InfoContext(ctx, "run_ceiling", "agent", s.Agent, "schedule", s.ID, "runs", runs, "ceiling", ceiling)
		}
		stored, err := sv.Store.Get(ctx, s.Tenant, s.ID)
		if err != nil {
			return false, err
		}
		stored.Ceiling = &Ceiling{Day: day, Runs: runs}
		if err := sv.Store.Put(ctx, stored); err != nil {
			return false, err
		}
	}
	return true, nil
}

// TriggerNow fires a schedule with a check once, now, outside its
// timetable: the read first, and a run only when it finds something new, as
// any firing. start starts that run and returns at once (the caller runs it
// in the background), so the cursor moves once the run is started, not once
// it ends. outcome is what the firing was kept as: FiredRun when a run
// started, else why not (quiet, too large, halted, switching, waiting for
// credits); a check that couldn't read is FiredCheckFailed, with the read's
// reason in err.
func (sv *Service) TriggerNow(ctx context.Context, tenant, id string, start func(ctx context.Context, f Firing) error) (outcome string, err error) {
	s, err := sv.Store.Get(ctx, tenant, id)
	if err != nil {
		return "", err
	}
	now := sv.now()
	f := Firing{Tenant: tenant, Schedule: id, Agent: s.Agent, Input: s.Input, Due: now.Truncate(time.Second)}
	if s.AppCheck == nil || sv.Probe == nil {
		if err := start(ctx, f); err != nil {
			return "", err
		}
		return FiredRun, nil
	}
	once := *sv
	once.History = keptOutcome{History: sv.History, outcome: &outcome}
	once.Start = func(ctx context.Context, f Firing) error {
		err := start(ctx, f)
		if err == nil {
			outcome = FiredRun
		}
		return err
	}
	prev := *s.AppCheck
	err = once.start(ctx, s, f, now)
	if outcome == "" && err == nil {
		outcome = FiredRun // started, and its run kept no outcome of its own
	}
	if outcome == FiredRun && err == nil && !prev.Due.Equal(f.Due) {
		// Its run goes on in the background: what was handed is kept, so
		// the next firing can hand it again if that run ends having done
		// nothing.
		if stored, gerr := sv.Store.Get(ctx, tenant, id); gerr == nil && stored.AppCheck != nil {
			stored.AppCheck.Handed = &Handed{Run: f.Key(), Last: prev.Last, Partial: prev.Partial}
			if perr := sv.Store.Put(ctx, stored); perr != nil {
				return outcome, perr
			}
		}
	}
	return outcome, err
}

// settleHanded looks at the run a firing by hand handed s's items to. Ended
// having done nothing (or before it could, or cut short having changed
// nothing), its items go again: the cursor goes back to where it was before
// them. Ended otherwise, it is let go. Still going, it is kept, and this
// firing reads past the cursor as it stands.
func (sv *Service) settleHanded(ctx context.Context, s Schedule) (Schedule, error) {
	h := s.AppCheck.Handed
	if sv.RunEnded == nil {
		return s, nil
	}
	done, why := sv.RunEnded(ctx, s.Tenant, h.Run)
	if !done {
		return s, nil
	}
	c := *s.AppCheck
	if errors.Is(why, ErrUnworked) || errors.Is(why, ErrSwitching) || errors.Is(why, ErrUnfinished) {
		c.Last, c.Partial = h.Last, h.Partial
	}
	c.Handed = nil
	s.AppCheck = &c
	return s, sv.Store.Put(ctx, s)
}

// keptOutcome is a History that also says what the last firing it kept
// came to.
type keptOutcome struct {
	History
	outcome *string
}

func (k keptOutcome) Add(ctx context.Context, tenant, id string, f Fired) error {
	*k.outcome = f.Outcome
	if k.History == nil {
		return nil
	}
	return k.History.Add(ctx, tenant, id, f)
}

func (k keptOutcome) Firings(ctx context.Context, tenant, id string, limit int) ([]Fired, error) {
	if k.History == nil {
		return nil, nil
	}
	return k.History.Firings(ctx, tenant, id, limit)
}

// start runs the firing: for a schedule with a check, only when the read
// finds something new, with it in the run's input. The check's cursor moves
// only once the run has started; a repeated delivery of the firing that
// started it starts that run again (Firing.Key), not another read.
func (sv *Service) start(ctx context.Context, s Schedule, f Firing, now time.Time) error {
	if c := s.AppCheck; c != nil && c.Handed != nil && !c.Due.Equal(f.Due) {
		settled, err := sv.settleHanded(ctx, s)
		if err != nil {
			return err
		}
		s = settled
	}
	if c := s.AppCheck; c != nil && !c.Due.Equal(f.Due) {
		if sv.Probe == nil {
			return sv.keep(ctx, s, f, Fired{Due: f.Due, At: now, Outcome: FiredDropped})
		}
		result, err := sv.Probe(ctx, s.Tenant, s.Agent, *c)
		switch {
		case errors.Is(err, ErrHalted):
			// Nothing read, nothing runs: the cursor stays where it was.
			return sv.keep(ctx, s, f, Fired{Due: f.Due, At: now, Outcome: FiredHalted})
		case errors.Is(err, ErrSwitching):
			return sv.keep(ctx, s, f, Fired{Due: f.Due, At: now, Outcome: FiredSwitching})
		case err != nil:
			return sv.checkFailed(ctx, s, f, now, err)
		}
		// How often these items were handed already. A batch given up on is
		// behind the cursor: what comes now starts its own count.
		cur := *c
		if cur.Tries >= MaxCheckTries {
			cur.Tries = 0
		}
		input, next, n, tooLarge, err := cur.batch(s.Input, result)
		if err != nil {
			return sv.checkFailed(ctx, s, f, now, err)
		}
		if n == 0 {
			next.Tries = c.Tries              // nothing handed: the count says what it said
			if !reflect.DeepEqual(next, *c) { // the first check takes the cursor; a too-large item is passed
				s.AppCheck = &next
				if err := sv.Store.Put(ctx, s); err != nil {
					return err
				}
			}
			outcome := FiredQuiet
			if tooLarge > 0 {
				outcome = FiredTooLarge
			}
			return sv.keep(ctx, s, f, Fired{Due: f.Due, At: now, Outcome: outcome})
		}
		f.Input = input
		unfinished := false
		var credits *CreditsError
		if err := sv.Start(ctx, f); err != nil {
			switch {
			case errors.Is(err, ErrUnworked):
				// Its run did nothing: the items go back, as the cursor stays.
				return sv.unworked(ctx, s, f, now)
			case errors.As(err, &credits):
				// No run: the items wait, as the cursor stays.
				return sv.waited(ctx, s, f, now, credits.Why)
			case errors.Is(err, ErrSwitching):
				// No run: the items wait for the new version, as the cursor stays.
				return sv.keep(ctx, s, f, Fired{Due: f.Due, At: now, Outcome: FiredSwitching})
			case errors.Is(err, ErrHalted):
				// No run: the items wait for the halt's end, as the cursor stays.
				return sv.keep(ctx, s, f, Fired{Due: f.Due, At: now, Outcome: FiredHalted})
			case errors.Is(err, ErrUnfinished) && cur.Tries+1 < MaxCheckTries:
				// Its run didn't finish them and changed nothing: they go
				// again, as the cursor stays. Due names this firing, so its
				// repeated delivery counts once.
				cur.Tries, cur.Due = cur.Tries+1, f.Due
				s.AppCheck, s.Waited = &cur, nil
				if err := sv.Store.Put(ctx, s); err != nil {
					return err
				}
				return sv.keep(ctx, s, f, Fired{Due: f.Due, At: now, Outcome: FiredRetrying, Run: f.Key()})
			case errors.Is(err, ErrUnfinished):
				// Handed MaxCheckTries runs, none finished them: the cursor moves on.
				unfinished, next.Tries = true, MaxCheckTries
			case errors.Is(err, ErrNotAgain):
				// A call of its run may have changed something, or a turn of
				// it was declined: never handed again, so nothing is done
				// twice. The cursor moves on.
				unfinished, next.Tries = true, 0
			default:
				return err
			}
		} else {
			next.Tries = 0
		}
		next.Due = f.Due
		s.AppCheck = &next
		if unfinished {
			s.Waited = nil
			if err := sv.Store.Put(ctx, s); err != nil {
				return err
			}
			return sv.failed(ctx, s, f, Fired{Due: f.Due, At: now, Outcome: FiredUnfinished, Run: f.Key()})
		}
		s.Failing, s.Waited = nil, nil
		if err := sv.Store.Put(ctx, s); err != nil {
			return err
		}
		return sv.keep(ctx, s, f, Fired{Due: f.Due, At: now, Outcome: FiredRun, Run: f.Key()})
	}
	if err := sv.Start(ctx, f); err != nil {
		var credits *CreditsError
		started := false
		switch {
		case errors.Is(err, ErrUnworked):
			return sv.unworked(ctx, s, f, now)
		case errors.As(err, &credits):
			return sv.waited(ctx, s, f, now, credits.Why)
		case errors.Is(err, ErrHalted):
			return sv.keep(ctx, s, f, Fired{Due: f.Due, At: now, Outcome: FiredHalted})
		case errors.Is(err, ErrSwitching):
			return sv.keep(ctx, s, f, Fired{Due: f.Due, At: now, Outcome: FiredSwitching})
		case (errors.Is(err, ErrUnfinished) || errors.Is(err, ErrNotAgain)) && s.AppCheck != nil:
			// A repeated delivery of a check's firing whose run didn't
			// finish: kept as the first delivery kept it, and counted once.
			if errors.Is(err, ErrUnfinished) && s.AppCheck.Tries < MaxCheckTries {
				return sv.keep(ctx, s, f, Fired{Due: f.Due, At: now, Outcome: FiredRetrying, Run: f.Key()})
			}
			return sv.failed(ctx, s, f, Fired{Due: f.Due, At: now, Outcome: FiredUnfinished, Run: f.Key()})
		case errors.Is(err, ErrUnfinished), errors.Is(err, ErrNotAgain):
			// A plain schedule hands its run nothing to give back: a run
			// that started and worked, as it always was.
			started = true
		}
		if !started {
			return err
		}
	}
	if s.Failing != nil || s.Waited != nil {
		if err := sv.ranAgain(ctx, s); err != nil {
			return err
		}
	}
	return sv.keep(ctx, s, f, Fired{Due: f.Due, At: now, Outcome: FiredRun, Run: f.Key()})
}

// waited keeps a firing whose run KIFF's balance refused: FiredCredits in
// its history, and the schedule's Waited, so whoever reads the schedule
// sees why nothing ran. Its failing is left as it was. A check's cursor
// stays where it was, so the next firing that runs hands the same items. A
// once schedule (an agent's follow-up, say) is off after its firing, as
// always (keep): its work doesn't run again, and its Waited says so. The
// schedule is read again first, so an owner's change isn't written over.
func (sv *Service) waited(ctx context.Context, s Schedule, f Firing, now time.Time, why string) error {
	if err := sv.keepAs(ctx, f, Fired{Due: f.Due, At: now, Outcome: FiredCredits}); err != nil {
		return err
	}
	stored, err := sv.Store.Get(ctx, s.Tenant, s.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	stored.Waited = &Waited{Due: f.Due.UTC(), Why: why}
	if s.KindOf() == KindOnce {
		offOnce(&stored, f.Due)
	}
	return sv.Store.Put(ctx, stored)
}

// ranAgain unsets a schedule's failing and Waited: one of its runs started,
// and worked. The schedule is read again first.
func (sv *Service) ranAgain(ctx context.Context, s Schedule) error {
	stored, err := sv.Store.Get(ctx, s.Tenant, s.ID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	stored.Failing, stored.Waited = nil, nil
	return sv.Store.Put(ctx, stored)
}

// unworked keeps a firing whose run failed before any turn: one more in the
// schedule's run of such firings, counted once however often the firing is
// delivered, and FiredUnworked in its history. A check's cursor stays where it
// was, so the next firing hands the same items. A once schedule (an agent's
// follow-up, say) is off after its firing, as always (offOnce): its work
// doesn't run again, and its failing says so. The schedule is read again
// first, so an owner's change made while the run went isn't written over.
func (sv *Service) unworked(ctx context.Context, s Schedule, f Firing, now time.Time) error {
	return sv.failed(ctx, s, f, Fired{Due: f.Due, At: now, Outcome: FiredUnworked, Run: f.Key()})
}

// checkFailed keeps a firing whose check couldn't read the app: FiredCheckFailed
// in its history, counted toward the schedule's failing as a run that did no
// work is, with no run (none started) and the cursor where it was, so the next
// firing reads the same items again. The read's error is answered too, so the
// invocation logs it.
func (sv *Service) checkFailed(ctx context.Context, s Schedule, f Firing, now time.Time, read error) error {
	// Why, at the source, in one line: the agent, the check's tool and a fixed reason. Never the read's
	// text, the vendor's words, its ids or the tenant.
	if sv.Log != nil {
		tool := ""
		if s.AppCheck != nil {
			tool = s.AppCheck.Tool
		}
		sv.Log.InfoContext(ctx, "check_failed", "agent", s.Agent, "tool", tool, "reason", string(reasonOf(read)))
	}
	if err := sv.failed(ctx, s, f, Fired{Due: f.Due, At: now, Outcome: FiredCheckFailed}); err != nil {
		return errors.Join(read, err)
	}
	return fmt.Errorf("schedule: the check couldn't read the app (kept as %s): %w", FiredCheckFailed, read)
}

// failed keeps fired, a firing that did no work, and counts it toward the
// schedule's failing (Failing.Run is the firing's key, whether or not a run
// was made under it, so a repeated delivery is counted once).
func (sv *Service) failed(ctx context.Context, s Schedule, f Firing, fired Fired) error {
	stored, err := sv.Store.Get(ctx, s.Tenant, s.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return sv.keepAs(ctx, f, fired) // gone meanwhile: left gone
	case err != nil:
		return err
	}
	switch {
	case stored.Failing != nil && (stored.Failing.Run == f.Key() || !f.Due.After(stored.Failing.Last)):
		// A repeated delivery of a firing counted already, or a late one of
		// an older firing, delivered after a newer one was counted.
	case stored.Failing != nil:
		stored.Failing = &Failing{Since: stored.Failing.Since, Firings: stored.Failing.Firings + 1, Run: f.Key(), Outcome: fired.Outcome, Last: f.Due}
	default:
		stored.Failing = &Failing{Since: f.Due, Firings: 1, Run: f.Key(), Outcome: fired.Outcome, Last: f.Due}
	}
	if stored.Failing.Firings >= PauseAfter && stored.Enabled && stored.KindOf() != KindOnce {
		// Off until the owner switches it on again: nothing more spent on work that can't succeed.
		stored.Enabled, stored.Failing.Paused = false, true
		if sv.Log != nil {
			sv.Log.InfoContext(ctx, "schedule_paused", "agent", stored.Agent, "schedule", stored.ID, "failing", stored.Failing.Firings)
		}
	}
	offOnce(&stored, f.Due)
	if err := sv.Store.Put(ctx, stored); err != nil {
		return err
	}
	return sv.keepAs(ctx, f, fired)
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
	if err := sv.start(ctx, s, f, now); err != nil {
		return sv.putBack(ctx, w, err)
	}
	return nil
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
// history does too. It is turned off as stored now (offOnce).
func (sv *Service) keep(ctx context.Context, s Schedule, f Firing, fired Fired) error {
	if err := sv.keepAs(ctx, f, fired); err != nil {
		return err
	}
	if s.KindOf() != KindOnce {
		return nil
	}
	stored, err := sv.Store.Get(ctx, s.Tenant, s.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil // removed meanwhile: left removed
	case err != nil:
		return err
	case !offOnce(&stored, fired.Due):
		return nil
	}
	return sv.Store.Put(ctx, stored)
}

// offOnce turns s off when it is a once schedule that still names the firing
// due, and reports whether it did: its one firing came. s is the schedule as
// stored now, read again after the run, so what changed while it went (an
// owner's edit, its failing) is kept, and one its owner moved to another time
// meanwhile stays on for that time.
func offOnce(s *Schedule, due time.Time) bool {
	if s.KindOf() != KindOnce || !s.names(due) {
		return false
	}
	s.Enabled = false
	return true
}
