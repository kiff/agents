package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/kiff/agents/model"
)

// LeaseGrace is how long past its invocation's own deadline a claim lasts. A
// healthy run can't outlive its invocation, so it needs no heartbeat: once the
// lease has run out, the run's invocation is gone.
const LeaseGrace = time.Minute

// PendingTimeout is how long a PENDING run may wait for an invocation to claim
// it; past it, the sweep marks it ERROR. It stays well inside
// session.MaxStartDelay, so a run that is claimed can always start recording.
const PendingTimeout = 5 * time.Minute

// ErrNotFound means no such run for this tenant; ErrExists, that Create found
// one; ErrConflict, that the run changed since it was read (read it again);
// ErrTransition, that its status can't move as asked; ErrLeaseLost, that the
// invocation's lease ran out, so it may write nothing more; ErrNotReady, that a
// paused run still waits for a requirement; ErrCancelled, that a pending run
// was asked to stop, so it never starts (it is also ErrTransition).
var (
	ErrNotFound   = errors.New("run: not found")
	ErrExists     = errors.New("run: already exists")
	ErrConflict   = errors.New("run: changed meanwhile")
	ErrTransition = errors.New("run: status can't move so")
	ErrLeaseLost  = errors.New("run: lease ran out")
	ErrNotReady   = errors.New("run: a requirement is still unresolved")
	ErrFixed      = errors.New("run: what it is can't change")
	ErrCancelled  = errors.New("run: asked to stop")
)

// Same reports whether two versions of a run are the same run: the same
// tenant, ID, agent, session, trigger, source and creation time. None of
// those ever changes; stores list runs by them.
func Same(a, b Run) bool {
	return a.Tenant == b.Tenant && a.ID == b.ID && a.Agent == b.Agent && a.Session == b.Session &&
		a.Trigger == b.Trigger && a.Source == b.Source && a.Created.Equal(b.Created)
}

// moves is a run's life: where each status may go. Final statuses go nowhere.
var moves = map[Status][]Status{
	Pending: {Running, Cancelled, Error},
	Running: {Completed, Error, Cancelled, Paused},
	Paused:  {Running, Cancelled},
}

// CanMove reports whether a run may go from one status to another. Staying in
// a status that isn't final is allowed: a running run records its cost, a
// paused one an answer.
func CanMove(from, to Status) bool {
	if from == to {
		return !from.Final()
	}
	for _, s := range moves[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Due is when the sweep must look at the run, or zero: a RUNNING run when its
// lease runs out, a PENDING one when its wait for a claim does.
func (r Run) Due() time.Time {
	switch r.Status {
	case Running:
		return r.Lease
	case Pending:
		return r.Created.Add(PendingTimeout)
	}
	return time.Time{}
}

// Runs keeps runs. Every call names the tenant, so one tenant never reaches
// another's runs, except Due, which the sweep uses across tenants.
//
// A Runs doesn't decide how a run moves; this package does, and stores every
// change as a compare-and-set through Put. What has expired (an ended run past
// its Expires) is gone as far as callers can tell, by the store's own clock.
type Runs interface {
	// Create stores a new run (ErrExists if its ID is taken).
	Create(ctx context.Context, r Run) error
	// Get returns a run.
	Get(ctx context.Context, tenant, id string) (Run, error)
	// Put replaces prev, as read, with next, if the stored run is still at
	// prev.Version (ErrConflict otherwise). next.Version is prev.Version+1,
	// and next is the Same run as prev (ErrFixed otherwise).
	Put(ctx context.Context, prev, next Run) error
	// List returns an agent's runs, newest first, at most limit, without
	// their paused turn (Get has it).
	List(ctx context.Context, tenant, agent string, limit int) ([]Run, error)
	// ListSource returns the runs one source started, by trigger and
	// source (the runs a schedule started: BySchedule and its ID), newest
	// first, at most limit, without their paused turn. An empty source lists
	// nothing.
	ListSource(ctx context.Context, tenant string, trigger Trigger, source string, limit int) ([]Run, error)
	// Due returns runs whose Due is before now, across tenants, the oldest
	// due first, at most limit, without their paused turn.
	Due(ctx context.Context, now time.Time, limit int) ([]Run, error)
	// Delete removes a run; DeleteAgent, all of an agent's runs.
	Delete(ctx context.Context, tenant, id string) error
	DeleteAgent(ctx context.Context, tenant, agent string) error
	// ByHold returns the tenant's run that paused on the call KIFF holds as
	// exception (a requirement's Exception), for as long as the run exists:
	// after it ended too, when its requirements are gone. ErrNotFound when
	// no run has held it (or not yet: its pause isn't stored).
	ByHold(ctx context.Context, tenant, exception string) (Run, error)
}

// New is a PENDING run, ready to Create.
func New(tenant, agent, session string, trigger Trigger, input string, now time.Time) Run {
	now = now.UTC()
	return Run{ID: NewID(), Tenant: tenant, Agent: agent, Session: session, Trigger: trigger, Status: Pending,
		Input: input, Created: now, Updated: now, Version: 1}
}

// Change applies change to r, as read, and stores the result with a
// compare-and-set. The status may move only as CanMove allows. Leaving
// RUNNING ends the claim, and a final status drops the paused turn: it holds
// what tools returned, and exists only to continue the run. It returns the run
// as stored.
func Change(ctx context.Context, runs Runs, r Run, now time.Time, change func(*Run) error) (Run, error) {
	next := r.clone() // the change can't reach r, so Put sees both as they are
	if err := change(&next); err != nil {
		return r, err
	}
	if !Same(r, next) {
		return r, ErrFixed
	}
	if !CanMove(r.Status, next.Status) {
		return r, fmt.Errorf("%w: %s to %s", ErrTransition, r.Status, next.Status)
	}
	if next.Status != Running {
		next.Claim, next.Lease = "", time.Time{}
	}
	if next.Status.Final() {
		next.Paused = nil
	}
	next.Holds = HoldsOf(r, next)
	next.ID, next.Tenant, next.Agent, next.Session, next.Created = r.ID, r.Tenant, r.Agent, r.Session, r.Created
	next.Updated, next.Version = now.UTC(), r.Version+1
	if err := runs.Put(ctx, r, next); err != nil {
		return r, err
	}
	return next, nil
}

// clone is r with its own copy of everything a change could edit in place:
// the requirements and the paused turn, with their inputs and results.
// HoldsOf is every hold the runs have named: their Holds and their
// requirements' exceptions, in order, once each.
func HoldsOf(runs ...Run) []string {
	var out []string
	add := func(ex string) {
		if ex != "" && !slices.Contains(out, ex) {
			out = append(out, ex)
		}
	}
	for _, r := range runs {
		for _, ex := range r.Holds {
			add(ex)
		}
		for _, q := range r.Requirements {
			add(q.Exception)
		}
	}
	return out
}

func (r Run) clone() Run {
	c := r
	c.Holds = slices.Clone(r.Holds)
	c.Requirements = nil
	for _, q := range r.Requirements {
		q.Input = bytes.Clone(q.Input)
		c.Requirements = append(c.Requirements, q)
	}
	if r.Paused != nil {
		t := Turn{Reply: model.Message{Role: r.Paused.Reply.Role, Blocks: cloneBlocks(r.Paused.Reply.Blocks)}, Results: cloneBlocks(r.Paused.Results)}
		c.Paused = &t
	}
	return c
}

func cloneBlocks(blocks []model.Block) []model.Block {
	if blocks == nil {
		return nil
	}
	out := make([]model.Block, len(blocks))
	for i, b := range blocks {
		b.Input = bytes.Clone(b.Input)
		out[i] = b
	}
	return out
}

// Claim takes a run for the invocation named claim, whose own deadline is
// deadline: a PENDING run, or a PAUSED one whose requirements KIFF has all
// resolved. The run is RUNNING under a lease that ends LeaseGrace past the
// deadline. Of two invocations claiming one run, one wins; the other gets
// ErrConflict, or ErrTransition once the run has moved on.
func Claim(ctx context.Context, runs Runs, tenant, id, claim string, deadline, now time.Time) (Run, error) {
	r, err := runs.Get(ctx, tenant, id)
	if err != nil {
		return Run{}, err
	}
	if r.Status == Paused {
		for _, q := range r.Requirements {
			if !q.Resolved(now) {
				return r, ErrNotReady
			}
		}
	}
	if r.Status != Pending && r.Status != Paused {
		return r, fmt.Errorf("%w: claiming a %s run", ErrTransition, r.Status)
	}
	// A pending run asked to stop never starts. A paused one still goes on
	// once KIFF has resolved it: its held calls' outcomes are recorded (a call
	// KIFF forwards always has a record here), and it stops at its first
	// checkpoint. Otherwise a run cancelled while KIFF came to hold one of its
	// calls could never move again.
	if r.Cancel && r.Status == Pending {
		return r, fmt.Errorf("%w: %w", ErrTransition, ErrCancelled)
	}
	return Change(ctx, runs, r, now, func(n *Run) error {
		n.Status, n.Claim, n.Lease = Running, claim, deadline.UTC().Add(LeaseGrace)
		return nil
	})
}

// SaveTries bounds how many times Save writes a run that keeps changing
// under it while its claim still holds.
const SaveTries = 5

// Save is Change for the invocation that claimed r: it writes only while it
// holds the lease. A change it makes after its lease ran out, or after the run
// moved on without it, writes nothing (ErrLeaseLost).
//
// A run that changed since r was read but is still this invocation's is read
// again and change applied to it as stored, so change may run more than once,
// each time on the run as stored. Today the only writer that changes a RUNNING
// run under its claimant is a cancel (answers are recorded on PAUSED runs,
// which hold no claim). The run Save returns shows what changed meanwhile: a
// claimant that finds Cancel set ends the run CANCELLED at its next
// checkpoint. A pause onto a run asked to stop is stored, Cancel set: KIFF
// holds a call, so the run waits for it, is claimed again once KIFF has
// resolved it (see Claim), records the call's outcome, and ends CANCELLED.
// A run that still changes after SaveTries writes is ErrConflict.
func Save(ctx context.Context, runs Runs, r Run, now time.Time, change func(*Run) error) (Run, error) {
	for try := 1; ; try++ {
		if r.Status != Running || r.Claim == "" || !now.Before(r.Lease) {
			return r, ErrLeaseLost
		}
		next, err := Change(ctx, runs, r, now, change)
		if !errors.Is(err, ErrConflict) || try == SaveTries {
			return next, err
		}
		stored, gerr := runs.Get(ctx, r.Tenant, r.ID)
		switch {
		case errors.Is(gerr, ErrNotFound):
			return r, fmt.Errorf("%w: the run is gone", ErrLeaseLost)
		case gerr != nil:
			return r, gerr
		case stored.Claim != r.Claim:
			return r, fmt.Errorf("%w: %w", ErrLeaseLost, err)
		}
		r = stored
	}
}

// Sweeper marks ended the runs nobody is left to end: a RUNNING run whose
// lease ran out (its invocation crashed or timed out, and can't write ERROR
// itself), and a PENDING run no invocation claimed within PendingTimeout.
type Sweeper struct {
	Runs Runs
	// Ended ends what else a RUNNING run left open (its recording), before
	// the run is marked: if it fails, the run stays due, and the next sweep
	// tries again. It must accept a run whose recording has ended already.
	// A PENDING run has begun no recording, so it isn't called for one.
	Ended func(ctx context.Context, r Run) error
	// Expires is when a run the sweep ends may go, from its agent's keep
	// rules (nil, or zero: kept until deleted).
	Expires func(r Run) time.Time
}

// Sweep marks every run whose Due has passed, at most limit: ERROR, or
// CANCELLED if it was asked to stop. A run that moved on meanwhile is left
// alone. It returns how many runs it marked; errors are joined.
func (sw Sweeper) Sweep(ctx context.Context, now time.Time, limit int) (int, error) {
	due, err := sw.Runs.Due(ctx, now, limit)
	if err != nil {
		return 0, err
	}
	var errs []error
	marked := 0
	for _, d := range due {
		ok, err := sw.mark(ctx, d, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("run %s: %w", d.ID, err))
		}
		if ok {
			marked++
		}
	}
	return marked, errors.Join(errs...)
}

// mark ends one due run, read again whole: Due leaves out the paused turn,
// which a run continued from PAUSED still holds until its first Save.
func (sw Sweeper) mark(ctx context.Context, d Run, now time.Time) (bool, error) {
	r, err := sw.Runs.Get(ctx, d.Tenant, d.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	case r.Due().IsZero() || !r.Due().Before(now):
		return false, nil // it moved on meanwhile
	}
	if r.Status == Running && sw.Ended != nil {
		if err := sw.Ended(ctx, r); err != nil {
			return false, err
		}
	}
	_, err = Change(ctx, sw.Runs, r, now, func(n *Run) error {
		switch {
		case r.Cancel:
			n.Status = Cancelled
		case r.Status == Running:
			n.Status, n.Error = Error, "its lease ran out: the invocation crashed or timed out"
		default:
			n.Status, n.Error = Error, "no invocation claimed it in time"
		}
		if sw.Expires != nil {
			n.Expires = sw.Expires(r)
		}
		return nil
	})
	if errors.Is(err, ErrConflict) {
		return false, nil // it moved on meanwhile: not ours to mark
	}
	return err == nil, err
}

// Answer records KIFF's answer on the requirement whose hold is exception:
// when, and what (approved, refused, expired, invalidated). It returns the
// run as stored. Recording an answer again changes nothing, and so does an
// answer for a run that is no longer paused (it went on, or ended): the run
// moved on without it. A run that hasn't paused on that hold yet is
// ErrNotFound, so the alert is delivered again later.
func Answer(ctx context.Context, runs Runs, tenant, exception, answer string, now time.Time) (Run, error) {
	if exception == "" || answer == "" {
		return Run{}, errors.New("run: an answer names its hold and what KIFF answered")
	}
	for try := 1; ; try++ {
		r, err := runs.ByHold(ctx, tenant, exception)
		if err != nil {
			return r, err
		}
		i := slices.IndexFunc(r.Requirements, func(q Requirement) bool { return q.Exception == exception })
		if i < 0 || !r.Requirements[i].Answered.IsZero() || r.Status != Paused {
			// Nothing left to record it on: the run went on (resolved by
			// expiry, then claimed) or ended. Only a cancel changes a
			// running run under its claimant (Save).
			return r, nil
		}
		next, err := Change(ctx, runs, r, now, func(n *Run) error {
			n.Requirements[i].Answered, n.Requirements[i].Answer = now.UTC(), answer
			return nil
		})
		if !errors.Is(err, ErrConflict) || try == SaveTries {
			return next, err
		}
	}
}
