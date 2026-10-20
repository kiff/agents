// Package runner executes runs, as the server's Runner: it starts a run in its
// session, claims it under a lease, builds its agent for its tenant, records
// it, runs the loop and stores where it stopped. A paused run continues here
// too, under the same run ID, once KIFF has resolved what it waits for.
//
// A run doesn't stop when its caller goes away: the stream can drop, and the
// run carries on to its next stop, which resume picks back up. It stops at
// its invocation's deadline, inside its lease.
package runner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/chat"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool"
)

// MaxRun is how long a run may go when its caller set no deadline: Lambda's
// own limit.
const MaxRun = 15 * time.Minute

// StopTimeout bounds storing where a run stopped, which happens even when the
// run stopped because its deadline passed.
const StopTimeout = 10 * time.Second

// MaxHistory bounds how many finished runs of its session a run sees,
// whatever its definition asks.
const MaxHistory = 50

// What a run's Error says, for each way it fails. Run.Error is shown to the
// tenant, so it says what failed, never why: the cause can carry a backend's
// own diagnostics, and Execute returns it to its caller to log instead.
const (
	FailedBuild = "the agent could not be built"
	FailedStart = "the run could not be started"
	FailedRun   = "the run failed"
	FailedTime  = "the run reached its time limit"
)

// ErrNoCollect means a paused run can't be continued: no Collect is set to get
// its held calls' outcomes from KIFF.
var ErrNoCollect = errors.New("runner: a paused run can't continue: nothing collects its held calls' outcomes")

// Runner starts and executes runs on its stores. It is a server.Runner.
type Runner struct {
	Runs     run.Runs
	Sessions session.Store
	// Builder builds a run's agent for its tenant, each time it executes.
	Builder *registry.Builder
	// Collect gets the outcome of each call a paused run waits for, from KIFF
	// by its operation ID: one result per requirement, none still held. Nil
	// until KIFF's operation IDs are recorded (step 4): a paused run can't
	// continue without it.
	Collect func(ctx context.Context, a agent.Agent, held []run.Requirement) ([]agent.ToolResultBody, error)
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

var _ server.Runner = (*Runner)(nil)

func (rn *Runner) now() time.Time {
	if rn.Now != nil {
		return rn.Now().UTC()
	}
	return time.Now().UTC()
}

// Start stores a PENDING run of the tenant's agent: in s.Session, which must
// be one of the agent's sessions (session.ErrNotFound otherwise), or in a new
// session when it is empty: a chat for the owner's runs, a job session for
// the others. A run named by s.ID that exists already is returned as stored,
// with run.ErrExists, and nothing new is stored.
func (rn *Runner) Start(ctx context.Context, s server.Start) (run.Run, error) {
	if s.Tenant == "" || s.Agent == "" || s.Input == "" || s.Trigger == "" {
		return run.Run{}, errors.New("runner: a run needs a tenant, an agent, a trigger and an input")
	}
	if s.ID != "" { // named: started already, by an earlier delivery?
		switch r, err := rn.Runs.Get(ctx, s.Tenant, s.ID); {
		case err == nil:
			return r, run.ErrExists
		case !errors.Is(err, run.ErrNotFound):
			return run.Run{}, err
		}
	}
	trial := s.Trigger == run.ByTrial
	if !trial && (s.Version != 0 || s.Draft != "") {
		return run.Run{}, errors.New("runner: only a trial runs a version or a draft")
	}
	var d registry.Definition
	var err error
	if trial && s.Version > 0 {
		d, err = rn.Builder.Agents.GetVersion(ctx, s.Tenant, s.Agent, s.Version)
	} else {
		d, err = rn.Builder.Agents.Get(ctx, s.Tenant, s.Agent)
	}
	if err != nil {
		return run.Run{}, err
	}
	opened := false // a session Start opens is removed again if the run can't be stored
	if s.Session == "" {
		rec, err := session.NewRecorderAt(rn.Sessions, d.Keep, rn.now)
		if err != nil {
			return run.Run{}, err
		}
		kind := session.KindRun
		switch s.Trigger {
		case run.ByOwner:
			kind = session.KindChat
		case run.ByTrial:
			kind = session.KindTrial
		}
		sess, err := rec.Open(ctx, s.Tenant, s.Agent, kind)
		if err != nil {
			return run.Run{}, err
		}
		s.Session, opened = sess.ID, true
	} else {
		sess, _, err := rn.Sessions.Read(ctx, s.Tenant, s.Session)
		if err != nil {
			return run.Run{}, err
		}
		if sess.Agent != s.Agent {
			return run.Run{}, session.ErrNotFound
		}
	}
	r := run.New(s.Tenant, s.Agent, s.Session, s.Trigger, s.Input, rn.now())
	r.Source = s.Source
	r.AgentVersion = d.Version
	if trial {
		r.Draft = s.Draft
	}
	if s.ID != "" {
		r.ID = s.ID
	}
	if err := rn.Runs.Create(ctx, r); err != nil {
		if opened {
			if derr := rn.Sessions.Delete(context.WithoutCancel(ctx), s.Tenant, s.Session); derr != nil {
				err = errors.Join(err, fmt.Errorf("runner: removing the session it opened: %w", derr))
			}
		}
		if s.ID != "" && errors.Is(err, run.ErrExists) { // another delivery stored it meanwhile
			if stored, gerr := rn.Runs.Get(ctx, s.Tenant, s.ID); gerr == nil {
				return stored, err
			}
		}
		return run.Run{}, err
	}
	return r, nil
}

// Execute claims the run and runs it to its next stop, passing each event to
// emit (nil: none). A PENDING run starts from its input; a PAUSED one, once
// KIFF has resolved all it waits for, continues from the turn it paused in
// with its held calls' outcomes. It returns the run as stored at that stop.
//
// A run that fails is stored ERROR, and Execute returns the cause too. A run
// another invocation claimed first is left alone: run.ErrConflict, or
// run.ErrTransition once it has moved on.
func (rn *Runner) Execute(ctx context.Context, tenant, id string, emit func(agent.Event)) (run.Run, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = rn.now().Add(MaxRun)
	}
	// Claiming and preparing the run have the whole deadline, as does the
	// lease. Only the loop stops StopTimeout short of it, so storing where it
	// stopped has time before the invocation is gone.
	ctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer cancel()
	loop, cancelLoop := context.WithDeadline(ctx, deadline.Add(-StopTimeout))
	defer cancelLoop()

	r, err := rn.Runs.Get(ctx, tenant, id)
	if err != nil {
		return run.Run{}, err
	}
	paused := r.Status == run.Paused
	if paused && rn.Collect == nil {
		return r, ErrNoCollect
	}
	x := &execution{rn: rn, tenant: tenant, id: id, claim: run.NewID()}
	if r, err = run.Claim(ctx, rn.Runs, tenant, id, x.claim, deadline, rn.now()); err != nil {
		return r, err
	}

	// A trial runs its own version, its tools marked so its writes are only
	// decided and its notes kept as they are (kiff/tafs#107); any other run
	// runs the live one.
	version := 0
	if r.Trigger == run.ByTrial {
		ctx = run.WithTrial(ctx, run.Trial{Draft: r.Draft})
		loop = run.WithTrial(loop, run.Trial{Draft: r.Draft})
		version = r.AgentVersion
	}
	a, d, err := rn.Builder.BuildVersion(ctx, tenant, r.Agent, r.ID, version)
	if err != nil {
		return x.notRun(ctx, r, paused, d.Keep, FailedBuild, fmt.Errorf("building the agent: %w", err))
	}
	a.Cancel = x.cancelled
	a.Steer = x.steered
	sess, events, err := rn.Sessions.Read(ctx, tenant, r.Session)
	if err != nil {
		return x.notRun(ctx, r, paused, d.Keep, FailedStart, fmt.Errorf("reading its session: %w", err))
	}
	history, err := rn.history(ctx, sess, events, r, d.History)
	if err != nil {
		return x.notRun(ctx, r, paused, d.Keep, FailedStart, fmt.Errorf("reading its history: %w", err))
	}
	rec, err := session.NewRecorderAt(rn.Sessions, d.Keep, rn.now)
	if err != nil {
		return x.notRun(ctx, r, paused, d.Keep, FailedStart, err)
	}

	if !paused {
		recording, err := rec.Start(ctx, sess, r.ID, r.Created)
		if err != nil {
			return x.notRun(ctx, r, false, d.Keep, FailedStart, fmt.Errorf("starting its recording: %w", err))
		}
		out, msgs, err := agent.Run(loop, a, recording, history, r.Input, emit)
		out, err = x.more(loop, a, recording, msgs, out, err, emit)
		return x.stop(ctx, recording, d.Keep, out, err)
	}
	// The recording resumes before KIFF is asked: collecting forwards an
	// approved call, once, so nothing may fail between it and recording
	// what it returned.
	recording, err := rec.Resume(ctx, sess, r.ID)
	if err != nil {
		return x.notRun(ctx, r, true, d.Keep, FailedStart, fmt.Errorf("resuming its recording: %w", err))
	}
	held, err := rn.Collect(ctx, a, r.Requirements)
	if err != nil {
		return x.repause(ctx, recording, r, fmt.Errorf("collecting its held calls: %w", err))
	}
	for i := range held {
		for _, q := range r.Requirements {
			if q.CallID == held[i].CallID {
				held[i].Result = HeldOutcome(q, held[i].Result)
			}
		}
	}
	out, msgs, err := agent.Continue(loop, a, recording, history, r, held, emit)
	out, err = x.more(loop, a, recording, msgs, out, err, emit)
	return x.stop(ctx, recording, d.Keep, out, err)
}

// HeldOutcome is a collected held call's result, opening with what happened
// to the call. The model last saw KIFF hold it, and its Card, which the call
// was outside; a bare tool answer then reads as KIFF having sent a call it
// should have held. Two things are said apart: the owner's decision, from
// the answer recorded on the requirement, and the delivery, from KIFF's
// state on the collected result. The call is said to be sent only when KIFF
// forwarded it; with no answer recorded (no alert came), a forwarded call
// was approved, as KIFF sends a held call only then.
func HeldOutcome(q run.Requirement, res tool.Result) tool.Result {
	notSent := func(why string) tool.Result {
		res.Text = why + "\n\nKIFF said: " + res.Text
		res.IsError = true
		return res
	}
	switch q.Answer {
	case "refused", "rejected":
		return notSent("KIFF held this call for the owner, who refused it. It was not sent.")
	case "expired":
		return notSent("KIFF held this call for the owner, and the hold expired without an answer. It was not sent.")
	case "invalidated", "withdrawn":
		return notSent("KIFF held this call for the owner, and it was withdrawn because the run was asked to stop. It was not sent.")
	}
	approved := q.Answer == "approved"
	held := "KIFF held this call for the owner"
	if approved {
		held += ", who approved it"
	}
	switch res.State {
	case tool.StateRefused:
		if approved {
			return notSent(held + ", but KIFF refused it when it came to send it (its Card may have changed). It was not sent.")
		}
		return notSent(held + ", and it was not approved (refused, or the hold expired). It was not sent.")
	case "failed":
		return notSent(held + ". KIFF could not send it, so it was not sent.")
	case "unknown":
		res.Text = held + ". KIFF sent it, but the tool's response was lost: whether it took effect is unknown. Don't repeat it; check first.\n\nKIFF said: " + res.Text
		res.IsError = true
		return res
	}
	res.Text = "KIFF held this call for the owner, who approved it. KIFF then sent it, once. The tool answered:\n\n" + res.Text
	return res
}

// Ended ends what a run left open when its invocation is gone: its recording,
// so what was kept only until the run's end is dropped, and its session's
// retention starts again. It is the sweep's run.Sweeper.Ended, and accepts a
// run whose recording has ended already, or never began.
func (rn *Runner) Ended(ctx context.Context, r run.Run) error {
	var keep session.Policy
	switch d, err := rn.Builder.Agents.Get(ctx, r.Tenant, r.Agent); {
	case err == nil:
		keep = d.Keep
	case !errors.Is(err, registry.ErrNotFound): // a removed agent's sessions go with it
		return err
	}
	err := rn.Sessions.EndRun(ctx, r.Tenant, r.Session, r.ID, expires(keep, rn.now()))
	if errors.Is(err, session.ErrEnded) || errors.Is(err, session.ErrNotFound) {
		return nil
	}
	return err
}

// Expires is when a run the sweep ends at now may go: its agent's keep rules,
// as when the runner ends a run itself. It is the sweep's run.Sweeper.Expires,
// which can't fail, so an agent that can't be read (removed, or a failed
// read) gives UnknownKeep: the run is kept a bounded time, never for good.
func (rn *Runner) Expires(ctx context.Context, r run.Run, now time.Time) time.Time {
	d, err := rn.Builder.Agents.Get(ctx, r.Tenant, r.Agent)
	if err != nil {
		return expires(nil, now)
	}
	return expires(d.Keep, now)
}

// history is what the model gets before current: the owner's and the agent's
// messages of the session's last n finished runs created before it, in
// order. A run still going, or paused, is left out until it ends. Which runs
// came before is read from the runs themselves, not from their events: the
// keep rules may have dropped all of the current run's own while it was
// paused.
func (rn *Runner) history(ctx context.Context, sess session.Session, events []session.Event, current run.Run, n int) ([]model.Message, error) {
	n = min(n, MaxHistory)
	if n <= 0 {
		return nil, nil
	}
	var order []string // the session's runs, as SortEvents orders them
	for _, e := range events {
		if len(order) == 0 || order[len(order)-1] != e.Run {
			order = append(order, e.Run)
		}
	}
	finished := map[string]bool{}
	for i := len(order) - 1; i >= 0 && len(finished) < n; i-- {
		if order[i] == current.ID || slices.Contains(sess.Held, order[i]) {
			continue
		}
		r, err := rn.Runs.Get(ctx, sess.Tenant, order[i])
		if errors.Is(err, run.ErrNotFound) {
			continue // its run is gone: deleted, or expired before its events
		}
		if err != nil {
			return nil, err
		}
		if r.Status.Final() && r.Created.Before(current.Created) {
			finished[order[i]] = true
		}
	}
	var kept []session.Event
	for _, e := range events {
		if finished[e.Run] {
			kept = append(kept, e)
		}
	}
	return chat.Conversation(kept), nil
}

// failed is what the Error of a run whose loop failed with err says.
func failed(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return FailedTime
	}
	return FailedRun
}

// UnknownKeep is how long a run is kept once it has ended when its agent's
// keep rules are unknown (the agent was removed before the run executed), so
// nothing is kept for good by accident.
const UnknownKeep = 7 * 24 * time.Hour

// expires is when a run that ended at now may go, under keep (zero: kept
// until deleted); with no keep rules, after UnknownKeep.
func expires(keep session.Policy, now time.Time) time.Time {
	if keep == nil {
		return now.Add(UnknownKeep)
	}
	if d := keep.SessionRetention(); d > 0 {
		return now.Add(d)
	}
	return time.Time{}
}

// execution is one invocation's claim on a run.
type execution struct {
	rn         *Runner
	tenant, id string
	claim      string
	// seen is the run as the last read left it, for the next use of it: the
	// steer check right after a checkpoint, or storing the stop right after
	// the check once the agent answered. One read for both.
	seen    run.Run
	hasSeen bool
	// unread are the messages taken at the last checkpoint that no model
	// turn has read yet. A run that fails before one does gives them back
	// (stop), so the caller sends them on.
	unread []int
}

// cancelled is the loop's checkpoint: whether the run was asked to stop. A
// claim that no longer holds stops the run with run.ErrLeaseLost. A read that
// fails counts as not asked: a store's passing error shouldn't end a run, the
// next checkpoint reads again, and save still finds a lost lease.
func (x *execution) cancelled(ctx context.Context) (bool, error) {
	r, err := x.rn.Runs.Get(ctx, x.tenant, x.id)
	if err != nil {
		return false, nil
	}
	if r.Status != run.Running || r.Claim != x.claim || !x.rn.now().Before(r.Lease) {
		return false, run.ErrLeaseLost
	}
	// A turn ran since the last checkpoint: what it took has been read.
	x.seen, x.hasSeen, x.unread = r, true, nil
	return r.Cancel, nil
}

// steered is the loop's other checkpoint: what the owner wrote to the run
// while it went and it hasn't taken, now marked taken, in order. It is
// marked taken before the loop records it, so a message is never taken
// twice; the run then sees it at its next model turn. A read or a write that
// fails otherwise (a passing store error, or the owner writing faster than
// the write's tries) takes nothing: the next checkpoint tries again. A lost
// claim stops the run, as at Cancel.
func (x *execution) steered(ctx context.Context) ([]string, error) {
	// The read just made: the checkpoint's, or the one once the agent answered.
	r, ok := x.take()
	if !ok {
		var err error
		if r, err = x.rn.Runs.Get(ctx, x.tenant, x.id); err != nil {
			return nil, nil
		}
	}
	if len(r.Untaken()) == 0 {
		return nil, nil
	}
	var took []string
	var at []int
	_, err := x.save(ctx, func(n *run.Run) error {
		took, at = nil, nil // the change may be applied again, to the run as stored
		for i := range n.Steering {
			if !n.Steering[i].Taken {
				n.Steering[i].Taken = true
				took, at = append(took, n.Steering[i].Text), append(at, i)
			}
		}
		return nil
	})
	switch {
	case err == nil:
		x.unread = append(x.unread, at...)
		return took, nil
	case errors.Is(err, run.ErrLeaseLost), errors.Is(err, run.ErrNotFound):
		return nil, err
	}
	return nil, nil
}

// saveStop is save for the writes a run's outcome depends on (where it
// stopped): an error that may pass (a throttled or dropped write) is tried
// again, backing off, until ctx ends. One that won't pass (the lease is lost,
// the run moved on or is gone) returns at once.
func (x *execution) saveStop(ctx context.Context, change func(*run.Run) error) (run.Run, error) {
	wait := 50 * time.Millisecond
	for {
		r, err := x.save(ctx, change)
		if err == nil || lasting(err) {
			return r, err
		}
		select {
		case <-ctx.Done():
			return r, err
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Second)
	}
}

// lasting reports whether a save's error won't pass by trying again.
func lasting(err error) bool {
	for _, e := range []error{run.ErrLeaseLost, run.ErrConflict, run.ErrTransition, run.ErrNotFound, run.ErrFixed, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// take is the run as the last read left it, once: what the next step reads
// instead of reading again.
func (x *execution) take() (run.Run, bool) {
	r, ok := x.seen, x.hasSeen
	x.seen, x.hasSeen = run.Run{}, false
	return r, ok
}

// more gives the agent's answer one last look before the run stops (#102):
// a message the owner wrote while it answered is taken, and gets a turn. It
// reads the run once, and storing the stop uses that read, so a run that
// nobody steers reads no more than before. A message that comes after it is
// left on the run, not taken, for the caller to send.
func (x *execution) more(ctx context.Context, a agent.Agent, rec *session.Recording, msgs []model.Message, out agent.Outcome, err error, emit func(agent.Event)) (agent.Outcome, error) {
	for err == nil && out.Stop == agent.Answered {
		r, gerr := x.rn.Runs.Get(ctx, x.tenant, x.id)
		if gerr != nil {
			return out, nil
		}
		x.seen, x.hasSeen = r, true
		// Asked to stop while it answered: the answer stands, and a message
		// stays not taken, for the caller.
		if r.Cancel || len(r.Untaken()) == 0 {
			return out, nil
		}
		turns := out.Turns
		out, msgs, err = agent.More(ctx, a, rec, msgs, out, emit)
		if out.Turns == turns { // nothing taken (no turn left, or a passing failure)
			return out, err
		}
	}
	return out, err
}

// save stores change on the run while this invocation holds it. The
// execution keeps no copy of the run, so it reads it first, unless the step
// before it just did; run.Save then reads it again if a cancel or a message
// changes it before the write.
func (x *execution) save(ctx context.Context, change func(*run.Run) error) (run.Run, error) {
	r, ok := x.take()
	if !ok {
		var err error
		if r, err = x.rn.Runs.Get(ctx, x.tenant, x.id); err != nil {
			return r, err
		}
	}
	if r.Claim != x.claim {
		return r, run.ErrLeaseLost
	}
	return run.Save(ctx, x.rn.Runs, r, x.rn.now(), change)
}

// stop stores where the loop stopped. The recording is paused or ended first:
// if that fails, the run stays RUNNING, and once its lease runs out the sweep
// ends the recording (Ended) and the run with it. A run is never stored as
// ended while what it kept only until its end is still kept.
//
// A run whose calls KIFF holds pauses even if the turn after them failed (the
// agent saying what it waits for, recorded as an Error event): the holds are
// real, and only a paused run can be continued once KIFF resolves them.
func (x *execution) stop(ctx context.Context, rec *session.Recording, keep session.Policy, out agent.Outcome, runErr error) (run.Run, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), StopTimeout)
	defer cancel()
	now := x.rn.now()
	if out.Stop == agent.Paused && out.Turn != nil {
		if err := rec.Pause(ctx); err != nil {
			return x.current(ctx, fmt.Errorf("runner: pausing the recording: %w", err))
		}
		// Each requirement's hold expiry is KIFF's to report (step 4). Until
		// then it is zero, and the requirement resolves only once answered.
		r, err := x.saveStop(ctx, func(n *run.Run) error {
			n.Status, n.Stop, n.Output, n.Error = run.Paused, string(out.Stop), out.Text, ""
			n.Cost, n.Turns, n.Requirements, n.Paused = out.Cost, out.Turns, out.Requirements, out.Turn
			n.Retries, n.Retry = 0, time.Time{} // a new pause: the sweep's backoff starts again
			return nil
		})
		return r, errors.Join(runErr, err) // a failed turn after the hold is still the caller's to log
	}
	if err := rec.End(ctx); err != nil {
		return x.current(ctx, errors.Join(runErr, fmt.Errorf("runner: ending the recording: %w", err)))
	}
	r, err := x.saveStop(ctx, func(n *run.Run) error {
		n.Cost, n.Turns, n.Requirements, n.Expires = out.Cost, out.Turns, nil, expires(keep, now)
		switch {
		case runErr != nil:
			n.Status, n.Stop, n.Error = run.Error, "", failed(runErr)
			// Taken, but no turn read them: given back, for the caller to send on.
			for _, i := range x.unread {
				if i < len(n.Steering) {
					n.Steering[i].Taken = false
				}
			}
		case out.Stop == agent.Cancelled:
			n.Status, n.Stop, n.Output = run.Cancelled, string(out.Stop), out.Text
		default:
			n.Status, n.Stop, n.Output, n.Refusal = run.Completed, string(out.Stop), out.Text, out.Refusal
		}
		return nil
	})
	return r, errors.Join(runErr, err)
}

// notRun stores a run that stopped before its loop began, because of cause. A
// paused one goes back to PAUSED, still waiting for what KIFF holds; a
// pending one is ERROR, saying public. Nothing was recorded either way.
func (x *execution) notRun(ctx context.Context, r run.Run, paused bool, keep session.Policy, public string, cause error) (run.Run, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), StopTimeout)
	defer cancel()
	if paused {
		next, err := x.saveStop(ctx, func(n *run.Run) error { n.Status = run.Paused; return nil })
		return next, errors.Join(cause, err)
	}
	next, err := x.saveStop(ctx, func(n *run.Run) error {
		n.Status, n.Error, n.Expires = run.Error, public, expires(keep, x.rn.now())
		return nil
	})
	return next, errors.Join(cause, err)
}

// repause puts a resumed run back to PAUSED: its recording holds it again, and
// it waits as before.
func (x *execution) repause(ctx context.Context, rec *session.Recording, r run.Run, cause error) (run.Run, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), StopTimeout)
	defer cancel()
	if err := rec.Pause(ctx); err != nil {
		return x.current(ctx, errors.Join(cause, err))
	}
	return x.notRun(ctx, r, true, nil, "", cause)
}

// current is the run as stored, with err.
func (x *execution) current(ctx context.Context, err error) (run.Run, error) {
	r, gerr := x.rn.Runs.Get(ctx, x.tenant, x.id)
	return r, errors.Join(err, gerr)
}
