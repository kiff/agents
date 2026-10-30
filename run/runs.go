package run

import (
	"bytes"
	"context"
	"encoding/json"
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

// ResolvedGrace is how long after its last requirement resolved a PAUSED run
// is due: time for KIFF to record a hold's expiry, so collecting finds it
// final, and for an alert's own continue to claim the run first.
const ResolvedGrace = time.Minute

// MaxRetryWait is the longest the sweep waits between continues of a
// resolved PAUSED run that stays paused.
const MaxRetryWait = time.Hour

// PausedGiveUp is how long the sweep keeps continuing a resolved PAUSED run
// that stays paused (its agent is gone, or KIFF keeps saying "not final")
// before it marks the run ERROR.
const PausedGiveUp = 24 * time.Hour

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
	// ErrFull means a run holds as many messages as it takes while it goes
	// (MaxSteering, or MaxSteeringBytes of text).
	ErrFull = errors.New("run: no room for another message")
)

// MaxSteering and MaxSteeringBytes bound what one run takes while it goes:
// the messages, and their text together. Past either, the owner waits for
// the run to end, or stops it. The bytes keep a run well inside a store's
// item (DynamoDB's is 400 KB), with its input, output and requirements.
const (
	MaxSteering      = 20
	MaxSteeringBytes = 64_000
)

// Same reports whether two versions of a run are the same run: the same
// tenant, ID, agent, session, trigger, source, creation time and where it is
// announced. None of those ever changes; stores list runs by most of them.
func Same(a, b Run) bool {
	return a.Tenant == b.Tenant && a.ID == b.ID && a.Agent == b.Agent && a.Session == b.Session &&
		a.Trigger == b.Trigger && a.Source == b.Source && a.Created.Equal(b.Created) && a.Notify == b.Notify
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

// Active reports whether the run holds one of its tenant's places: PENDING
// or RUNNING. A PAUSED run waits on KIFF and holds none.
func (r Run) Active() bool { return r.Status == Pending || r.Status == Running }

// Due is when the sweep must look at the run, or zero: a RUNNING run when its
// lease runs out, a PENDING one when its wait for a claim does, and a PAUSED
// one ResolvedGrace after its last requirement is answered or expires, or at
// its Retry if the sweep has continued it already. A
// paused run with an unanswered requirement that never expires is never due:
// only an answer resolves it.
func (r Run) Due() time.Time {
	switch r.Status {
	case Running:
		return r.Lease
	case Pending:
		return r.Created.Add(PendingTimeout)
	case Paused:
		at := r.resolved()
		if at.IsZero() {
			return time.Time{}
		}
		if due := at.Add(ResolvedGrace); !r.Retry.After(due) {
			return due
		}
		return r.Retry
	}
	return time.Time{}
}

// resolved is when the last of the run's requirements was answered or
// expires, or zero: no requirements, or one with neither.
func (r Run) resolved() time.Time {
	if len(r.Requirements) == 0 && r.Resumes > 0 {
		return r.Updated // a crashed run whose acting turn had all its results
	}
	var last time.Time
	for _, q := range r.Requirements {
		at := q.Answered
		if at.IsZero() {
			at = q.Expires
		}
		if at.IsZero() {
			return time.Time{}
		}
		if at.After(last) {
			last = at
		}
	}
	return last
}

// retryWait is how long the sweep waits after its nth continue of a run.
func retryWait(n int) time.Duration {
	if n > 10 {
		return MaxRetryWait
	}
	return min(ResolvedGrace<<n, MaxRetryWait)
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
	// List returns an agent's runs, newest first, at most limit (0 or
	// less: all of them), without their paused turn (Get has it).
	List(ctx context.Context, tenant, agent string, limit int) ([]Run, error)
	// ListSource returns the runs one source started, by trigger and
	// source (the runs a schedule started: BySchedule and its ID), newest
	// first, at most limit, without their paused turn. An empty source lists
	// nothing.
	ListSource(ctx context.Context, tenant string, trigger Trigger, source string, limit int) ([]Run, error)
	// ListAll returns runs across tenants, newest first, at most limit,
	// without their paused turn. It is for operators' read-only routes only:
	// every other read names its tenant.
	ListAll(ctx context.Context, limit int) ([]Run, error)
	// ListPaused is ListAll for PAUSED runs only.
	ListPaused(ctx context.Context, limit int) ([]Run, error)
	// Due returns RUNNING and PENDING runs whose Due is before now, across
	// tenants, the oldest due first, at most limit, without their paused turn.
	Due(ctx context.Context, now time.Time, limit int) ([]Run, error)
	// DuePaused is Due for PAUSED runs.
	DuePaused(ctx context.Context, now time.Time, limit int) ([]Run, error)
	// CountActive returns how many of the tenant's runs are Active: what
	// the server's per-tenant cap counts.
	CountActive(ctx context.Context, tenant string) (int, error)
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
		next.Claim, next.Lease, next.Acting = "", time.Time{}, nil
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

// clone is r with its own copy of everything a change could edit in place:
// every slice it holds, with their inputs and results, and the paused turn.
// TestCloneSharesNothingAChangeCouldEdit checks it field by field.
func (r Run) clone() Run {
	c := r
	c.Attachments = slices.Clone(r.Attachments)
	c.Holds = slices.Clone(r.Holds)
	c.NotAgain = nil
	for _, k := range r.NotAgain {
		k.Input = bytes.Clone(k.Input)
		c.NotAgain = append(c.NotAgain, k)
	}
	c.Done = nil
	for _, d := range r.Done {
		d.Input = bytes.Clone(d.Input)
		c.Done = append(c.Done, d)
	}
	c.Steering = slices.Clone(r.Steering)
	c.Requirements = nil
	for _, q := range r.Requirements {
		q.Input, q.Result = bytes.Clone(q.Input), bytes.Clone(q.Result)
		c.Requirements = append(c.Requirements, q)
	}
	// Every part of a turn, Before included: a copy without it would give the
	// continued run no memory of its earlier turns (the R-3005 test).
	c.Paused, c.Acting = r.Paused.clone(), r.Acting.clone()
	return c
}

// clone is a deep copy of t, or nil.
func (t *Turn) clone() *Turn {
	if t == nil {
		return nil
	}
	c := Turn{Reply: model.Message{Role: t.Reply.Role, Blocks: cloneBlocks(t.Reply.Blocks)}, Results: cloneBlocks(t.Results)}
	for _, m := range t.Before {
		c.Before = append(c.Before, model.Message{Role: m.Role, Blocks: cloneBlocks(m.Blocks)})
	}
	return &c
}

func cloneBlocks(blocks []model.Block) []model.Block {
	if blocks == nil {
		return nil
	}
	out := make([]model.Block, len(blocks))
	for i, b := range blocks {
		b.Input, b.Bytes = bytes.Clone(b.Input), bytes.Clone(b.Bytes)
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
// run under its claimant is a cancel or a message the owner wrote (Steer);
// answers are recorded on PAUSED runs, which hold no claim. The run Save returns shows what changed meanwhile: a
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

// Steer gives a run that hasn't ended, and wasn't asked to stop, a message
// the owner wrote while it goes. The run takes it at its next checkpoint,
// before a model turn: a running run as it goes, a pending one when it
// starts, a paused one when it continues. A run that ended, or was asked to
// stop, is ErrTransition: the caller sends the text as a new run, as for any
// message. It returns the run as stored.
func Steer(ctx context.Context, runs Runs, tenant, id, text string, now time.Time) (Run, error) {
	return SteerBy(ctx, runs, tenant, id, text, Requester{}, now)
}

// SteerBy is Steer, the message written by q (#219).
func SteerBy(ctx context.Context, runs Runs, tenant, id, text string, q Requester, now time.Time) (Run, error) {
	for try := 1; ; try++ {
		r, err := runs.Get(ctx, tenant, id)
		if err != nil {
			return r, err
		}
		next, err := Change(ctx, runs, r, now, func(n *Run) error {
			switch {
			case n.Status.Final() || n.Cancel:
				return fmt.Errorf("%w: the run has ended, or was asked to stop", ErrTransition)
			case len(n.Steering) >= MaxSteering || steeringBytes(n.Steering)+len(text) > MaxSteeringBytes:
				return ErrFull
			}
			n.Steering = append(n.Steering, Steering{Text: text, At: now.UTC(), Requester: q})
			return nil
		})
		if errors.Is(err, ErrConflict) && try < SaveTries {
			continue
		}
		return next, err
	}
}

// MaxDoneBytes bounds the inputs a run keeps with what it did (Done), all
// together, as MaxSteeringBytes bounds its messages: inside a store's item
// with the rest of the run. Past it, a call is still listed, without its
// input, so the list of what went through stays whole.
const MaxDoneBytes = 64_000

// AddDone is have with more after it, each keeping its input while the
// inputs together stay within MaxDoneBytes; past that, without it.
func AddDone(have []Done, more ...Done) []Done {
	n := 0
	for _, d := range have {
		n += len(d.Input)
	}
	for _, d := range more {
		if n+len(d.Input) > MaxDoneBytes {
			d.Input = nil
		}
		n += len(d.Input)
		have = append(have, d)
	}
	return have
}

func steeringBytes(s []Steering) int {
	n := 0
	for _, m := range s {
		n += len(m.Text)
	}
	return n
}

// Sweeper marks ended the runs nobody is left to end: a RUNNING run whose
// lease ran out (its invocation crashed or timed out, and can't write ERROR
// itself), and a PENDING run no invocation claimed within PendingTimeout. It
// continues a PAUSED run whose requirements have all resolved, in case no
// alert did (a hold that expired, or an alert's continue that failed), and
// marks one that is still paused PausedGiveUp later.
type Sweeper struct {
	Runs Runs
	// Ended ends what else a RUNNING or PAUSED run left open (its recording),
	// before the run is marked: if it fails, the run stays due, and the next
	// sweep tries again. It must accept a run whose recording has ended
	// already. A PENDING run has begun no recording, so it isn't called for one.
	Ended func(ctx context.Context, r Run) error
	// Continue executes a resolved PAUSED run, in the background: it claims
	// the run, collects its held calls' outcomes and carries on. Nil: paused
	// runs are left to their alerts until PausedGiveUp. A run that stays
	// paused is continued again, after a wait that doubles (Retry).
	Continue func(ctx context.Context, tenant, id string) error
	// Expires is when a run the sweep ends may go, from its agent's keep
	// rules (nil, or zero: kept until deleted).
	Expires func(r Run) time.Time
	// Hold holds the recording of a RUNNING run whose invocation stopped
	// while it acted (its lease ran out with an Acting turn), so a continue
	// can resume it. With Hold and Continue both set, such a run is carried
	// on from its acting turn, up to MaxResumes times, instead of marked
	// ERROR. It must accept a recording held already.
	Hold func(ctx context.Context, r Run) error
}

// MaxResumes is how many times the sweep carries a crashed run on from its
// acting turn, after the wait ResumeWaits gives each: a turn that stops its
// invocation every time isn't restarted three times in a few minutes. Past
// it, the run ends ERROR with NeedsYou, for the owner to look at.
const MaxResumes = 3

// ResumeWaits is how long the sweep waits before each restart of a crashed
// run, from when it finds it stopped.
var ResumeWaits = [MaxResumes]time.Duration{5 * time.Minute, 20 * time.Minute, time.Hour}

// NeedsYou is the error of a run the sweep carried on MaxResumes times: its
// note says needs_you, with it as the reason.
const NeedsYou = "It stopped 3 times while it worked, and was restarted each time from its last safe step. It needs you to look at it."

// Restarted is the reason a crashed run's note gives while it waits for its
// next restart.
const Restarted = "It stopped while it worked. It carries on from its last safe step, and nothing it did is done twice."

// Sweep looks at every run whose Due has passed: at most limit RUNNING and
// PENDING runs, which it marks ERROR (or CANCELLED if asked to stop), and
// then at most limit PAUSED ones, which it continues, or past PausedGiveUp
// marks. The two are read apart, so paused runs never keep crash recovery
// waiting. A run that moved on meanwhile is left alone. It returns how many
// runs it marked or continued; errors are joined.
func (sw Sweeper) Sweep(ctx context.Context, now time.Time, limit int) (int, error) {
	var errs []error
	done := 0
	for _, due := range []func(context.Context, time.Time, int) ([]Run, error){sw.Runs.Due, sw.Runs.DuePaused} {
		runs, err := due(ctx, now, limit)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, d := range runs {
			ok, err := sw.mark(ctx, d, now)
			if err != nil {
				errs = append(errs, fmt.Errorf("run %s: %w", d.ID, err))
			}
			if ok {
				done++
			}
		}
	}
	return done, errors.Join(errs...)
}

// SweepClaim is the claim the sweep holds on a PAUSED run it gives up on,
// while it ends the run's recording.
const SweepClaim = "sweep"

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
	case r.Status == Paused && now.Before(r.resolved().Add(ResolvedGrace+PausedGiveUp)):
		return sw.retry(ctx, r, now)
	case r.Status == Running && !r.Cancel && r.Acting != nil && sw.Hold != nil && sw.Continue != nil && r.Resumes < MaxResumes:
		return sw.resume(ctx, r, now)
	}
	paused := r.Status == Paused
	if paused {
		// Claimed first, as of the run read: a continue that claimed it
		// since wins, and its recording is never ended under it.
		r, err = Change(ctx, sw.Runs, r, now, func(n *Run) error {
			n.Status, n.Claim, n.Lease = Running, SweepClaim, now.UTC().Add(LeaseGrace)
			return nil
		})
		switch {
		case errors.Is(err, ErrConflict):
			return false, nil
		case err != nil:
			return false, err
		}
	}
	if r.Status != Pending && sw.Ended != nil {
		if err := sw.Ended(ctx, r); err != nil {
			return false, err // a paused run's claim runs out, and the next sweep marks it
		}
	}
	end := func(n *Run) error {
		switch {
		case r.Cancel:
			n.Status = Cancelled
		case paused:
			n.Status, n.Error = Error, "it could not continue once KIFF resolved its held calls"
		case r.Status == Running && r.Acting != nil && r.Resumes >= MaxResumes:
			n.Status, n.Error = Error, NeedsYou
		case r.Status == Running:
			n.Status, n.Error = Error, "its lease ran out: the invocation crashed or timed out"
		default:
			n.Status, n.Error = Error, "no invocation claimed it in time"
		}
		if sw.Expires != nil {
			n.Expires = sw.Expires(r)
		}
		return nil
	}
	if paused {
		_, err = Save(ctx, sw.Runs, r, now, end)
	} else {
		_, err = Change(ctx, sw.Runs, r, now, end)
	}
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrLeaseLost) {
		return false, nil // it moved on meanwhile: not ours to mark
	}
	return err == nil, err
}

// resume carries on a RUNNING run whose invocation stopped while it acted:
// its recording held, it is stored PAUSED on its acting turn (resumeOf),
// already resolved, with its next look (Retry) ResumeWaits away. Its note
// says it's retrying, and when. The sweep then continues it as any resolved
// paused run, and continuing repeats the turn's calls that have no result
// by their operation IDs.
func (sw Sweeper) resume(ctx context.Context, r Run, now time.Time) (bool, error) {
	if err := sw.Hold(ctx, r); err != nil {
		return false, err // still due: the next sweep tries again
	}
	_, err := Change(ctx, sw.Runs, r, now, func(n *Run) error {
		n.Status, n.Retries, n.Retry = Paused, 0, now.UTC().Add(ResumeWaits[n.Resumes])
		n.Resumes++
		n.Paused, n.Requirements = resumeOf(*n.Acting, now)
		return nil
	})
	switch {
	case errors.Is(err, ErrConflict):
		return false, nil // it moved on meanwhile
	case err != nil:
		return false, err
	}
	return true, nil
}

// Retrying reports whether r is a crashed run waiting for its next restart:
// paused on its acting turn by the sweep, not by a hold or a card.
func (r Run) Retrying() bool {
	if r.Status != Paused || r.Resumes == 0 {
		return false
	}
	for _, q := range r.Requirements {
		if !q.Resume() {
			return false
		}
	}
	return true
}

// resumeOf is the paused turn and requirements a crashed run carries on
// from, from its acting turn t: the results it stored stay results, and each
// call without one is a KindResume requirement, resolved at now.
func resumeOf(t Turn, now time.Time) (*Turn, []Requirement) {
	have := map[string]bool{}
	for _, b := range t.Results {
		have[b.CallID] = true
	}
	var reqs []Requirement
	for _, b := range t.Reply.Blocks {
		if b.Kind == model.ToolUse && !have[b.CallID] {
			reqs = append(reqs, Requirement{CallID: b.CallID, Tool: b.Tool, Input: b.Input, Kind: KindResume, Expires: now.UTC()})
		}
	}
	return &t, reqs
}

// retry pushes a resolved PAUSED run's next look out, then hands it to
// Continue. The next look is stored first: if the run stays paused, or
// nothing continues it, it waits longer each time, and runs due after it
// are swept.
func (sw Sweeper) retry(ctx context.Context, r Run, now time.Time) (bool, error) {
	r, err := Change(ctx, sw.Runs, r, now, func(n *Run) error {
		n.Retries++
		n.Retry = now.Add(retryWait(n.Retries))
		return nil
	})
	switch {
	case errors.Is(err, ErrConflict):
		return false, nil // it moved on meanwhile
	case err != nil:
		return false, err
	case sw.Continue == nil:
		return false, nil
	}
	if err := sw.Continue(ctx, r.Tenant, r.ID); err != nil {
		return false, err
	}
	return true, nil
}

// ErrNotOpen means no card of the run waits for an answer by that call: it
// was answered already, it stopped waiting, or the run went on.
var ErrNotOpen = errors.New("run: no card waits for that answer")

// AnswerCard records result as the answer to the run's card shown by call,
// once: the card's requirement is answered, and the run, paused, can go on.
// A card already answered, or a run no longer paused, is ErrNotOpen.
func AnswerCard(ctx context.Context, runs Runs, tenant, id, call string, result json.RawMessage, now time.Time) (Run, error) {
	for try := 1; ; try++ {
		r, err := runs.Get(ctx, tenant, id)
		if err != nil {
			return r, err
		}
		i := slices.IndexFunc(r.Requirements, func(q Requirement) bool { return q.Card() && q.CallID == call })
		if i < 0 || r.Status != Paused || r.Requirements[i].Resolved(now) {
			return r, ErrNotOpen
		}
		next, err := Change(ctx, runs, r, now, func(n *Run) error {
			n.Requirements[i].Answered, n.Requirements[i].Answer, n.Requirements[i].Result = now.UTC(), "answered", result
			return nil
		})
		if !errors.Is(err, ErrConflict) || try == SaveTries {
			return next, err
		}
	}
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
