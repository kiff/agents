package run

import (
	"context"
	"errors"
	"fmt"
	"time"
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
// paused run still waits for a requirement.
var (
	ErrNotFound   = errors.New("run: not found")
	ErrExists     = errors.New("run: already exists")
	ErrConflict   = errors.New("run: changed meanwhile")
	ErrTransition = errors.New("run: status can't move so")
	ErrLeaseLost  = errors.New("run: lease ran out")
	ErrNotReady   = errors.New("run: a requirement is still unresolved")
)

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
	// prev.Version (ErrConflict otherwise). next.Version is prev.Version+1.
	Put(ctx context.Context, prev, next Run) error
	// List returns an agent's runs, newest first, at most limit, without
	// their paused turn (Get has it).
	List(ctx context.Context, tenant, agent string, limit int) ([]Run, error)
	// Due returns runs whose Due is before now, across tenants, the oldest
	// due first, at most limit, without their paused turn.
	Due(ctx context.Context, now time.Time, limit int) ([]Run, error)
	// Delete removes a run; DeleteAgent, all of an agent's runs.
	Delete(ctx context.Context, tenant, id string) error
	DeleteAgent(ctx context.Context, tenant, agent string) error
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
	next := r
	next.Requirements = append([]Requirement(nil), r.Requirements...)
	if err := change(&next); err != nil {
		return r, err
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
	next.ID, next.Tenant, next.Agent, next.Session, next.Created = r.ID, r.Tenant, r.Agent, r.Session, r.Created
	next.Updated, next.Version = now.UTC(), r.Version+1
	if err := runs.Put(ctx, r, next); err != nil {
		return r, err
	}
	return next, nil
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
	if r.Cancel {
		return r, fmt.Errorf("%w: the run was cancelled", ErrTransition)
	}
	return Change(ctx, runs, r, now, func(n *Run) error {
		n.Status, n.Claim, n.Lease = Running, claim, deadline.UTC().Add(LeaseGrace)
		return nil
	})
}

// Save is Change for the invocation that claimed r: it writes only while it
// holds the lease. A change it makes after its lease ran out, or after the run
// moved on without it, writes nothing.
func Save(ctx context.Context, runs Runs, r Run, now time.Time, change func(*Run) error) (Run, error) {
	if r.Status != Running || r.Claim == "" || !now.Before(r.Lease) {
		return r, ErrLeaseLost
	}
	next, err := Change(ctx, runs, r, now, change)
	if errors.Is(err, ErrConflict) {
		return r, fmt.Errorf("%w: %w", ErrLeaseLost, err)
	}
	return next, err
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
