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
// the others.
func (rn *Runner) Start(ctx context.Context, s server.Start) (run.Run, error) {
	if s.Tenant == "" || s.Agent == "" || s.Input == "" || s.Trigger == "" {
		return run.Run{}, errors.New("runner: a run needs a tenant, an agent, a trigger and an input")
	}
	d, err := rn.Builder.Agents.Get(ctx, s.Tenant, s.Agent)
	if err != nil {
		return run.Run{}, err
	}
	if s.Session == "" {
		rec, err := session.NewRecorderAt(rn.Sessions, d.Keep, rn.now)
		if err != nil {
			return run.Run{}, err
		}
		kind := session.KindRun
		if s.Trigger == run.ByOwner {
			kind = session.KindChat
		}
		sess, err := rec.Open(ctx, s.Tenant, s.Agent, kind)
		if err != nil {
			return run.Run{}, err
		}
		s.Session = sess.ID
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
	if err := rn.Runs.Create(ctx, r); err != nil {
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
	ctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer cancel()

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

	a, d, err := rn.Builder.Build(ctx, tenant, r.Agent)
	if err != nil {
		return x.notRun(ctx, r, paused, d.Keep, fmt.Errorf("building the agent: %w", err))
	}
	a.Cancel = x.cancelled
	sess, events, err := rn.Sessions.Read(ctx, tenant, r.Session)
	if err != nil {
		return x.notRun(ctx, r, paused, d.Keep, fmt.Errorf("reading its session: %w", err))
	}
	history, err := rn.history(ctx, sess, events, r.ID, d.History)
	if err != nil {
		return x.notRun(ctx, r, paused, d.Keep, fmt.Errorf("reading its history: %w", err))
	}
	rec, err := session.NewRecorderAt(rn.Sessions, d.Keep, rn.now)
	if err != nil {
		return x.notRun(ctx, r, paused, d.Keep, err)
	}

	if !paused {
		recording, err := rec.Start(ctx, sess, r.ID, r.Created)
		if err != nil {
			return x.notRun(ctx, r, false, d.Keep, fmt.Errorf("starting its recording: %w", err))
		}
		out, _, err := agent.Run(ctx, a, recording, history, r.Input, emit)
		return x.stop(ctx, recording, d.Keep, out, err)
	}
	// The recording resumes before KIFF is asked: collecting forwards an
	// approved call, once, so nothing may fail between it and recording
	// what it returned.
	recording, err := rec.Resume(ctx, sess, r.ID)
	if err != nil {
		return x.notRun(ctx, r, true, d.Keep, fmt.Errorf("resuming its recording: %w", err))
	}
	held, err := rn.Collect(ctx, a, r.Requirements)
	if err != nil {
		return x.repause(ctx, recording, r, fmt.Errorf("collecting its held calls: %w", err))
	}
	out, _, err := agent.Continue(ctx, a, recording, history, r, held, emit)
	return x.stop(ctx, recording, d.Keep, out, err)
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

// history is what the model gets before run: the owner's and the agent's
// messages of the session's last n finished runs before it, in order. A run
// still going, or paused, is left out until it ends.
func (rn *Runner) history(ctx context.Context, sess session.Session, events []session.Event, current string, n int) ([]model.Message, error) {
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
	if i := slices.Index(order, current); i >= 0 {
		order = order[:i] // a continued run sees only what came before it
	}
	finished := map[string]bool{}
	for i := len(order) - 1; i >= 0 && len(finished) < n; i-- {
		if slices.Contains(sess.Held, order[i]) {
			continue
		}
		state, err := rn.Sessions.State(ctx, sess.Tenant, sess.ID, order[i])
		if err != nil {
			return nil, err
		}
		if state.Ended {
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

// expires is when a run that ended at now may go, under keep (zero: kept
// until deleted).
func expires(keep session.Policy, now time.Time) time.Time {
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
}

// cancelled is the loop's checkpoint: whether the run was asked to stop. A
// claim that no longer holds stops the run with run.ErrLeaseLost.
func (x *execution) cancelled(ctx context.Context) (bool, error) {
	r, err := x.rn.Runs.Get(ctx, x.tenant, x.id)
	if err != nil {
		return false, err
	}
	if r.Status != run.Running || r.Claim != x.claim || !x.rn.now().Before(r.Lease) {
		return false, run.ErrLeaseLost
	}
	return r.Cancel, nil
}

// save stores change on the run while this invocation holds it. The run is
// read again first: a cancel may have changed it since the claim.
func (x *execution) save(ctx context.Context, change func(*run.Run) error) (run.Run, error) {
	var r run.Run
	var err error
	for range 3 {
		if r, err = x.rn.Runs.Get(ctx, x.tenant, x.id); err != nil {
			return r, err
		}
		if r.Claim != x.claim {
			return r, run.ErrLeaseLost
		}
		var next run.Run
		if next, err = run.Save(ctx, x.rn.Runs, r, x.rn.now(), change); !errors.Is(err, run.ErrConflict) {
			return next, err
		}
		// It changed between the read and the write: read it again.
	}
	return r, err
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
		return x.save(ctx, func(n *run.Run) error {
			n.Status, n.Stop, n.Output, n.Error = run.Paused, string(out.Stop), out.Text, ""
			n.Cost, n.Turns, n.Requirements, n.Paused = out.Cost, out.Turns, out.Requirements, out.Turn
			return nil
		})
	}
	if err := rec.End(ctx); err != nil {
		return x.current(ctx, errors.Join(runErr, fmt.Errorf("runner: ending the recording: %w", err)))
	}
	r, err := x.save(ctx, func(n *run.Run) error {
		n.Cost, n.Turns, n.Requirements, n.Expires = out.Cost, out.Turns, nil, expires(keep, now)
		switch {
		case runErr != nil:
			n.Status, n.Stop, n.Error = run.Error, "", runErr.Error()
		case out.Stop == agent.Cancelled:
			n.Status, n.Stop, n.Output = run.Cancelled, string(out.Stop), out.Text
		default:
			n.Status, n.Stop, n.Output = run.Completed, string(out.Stop), out.Text
		}
		return nil
	})
	return r, errors.Join(runErr, err)
}

// notRun stores a run that stopped before its loop began, because of cause. A
// paused one goes back to PAUSED, still waiting for what KIFF holds; a
// pending one is ERROR. Nothing was recorded either way.
func (x *execution) notRun(ctx context.Context, r run.Run, paused bool, keep session.Policy, cause error) (run.Run, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), StopTimeout)
	defer cancel()
	if paused {
		next, err := x.save(ctx, func(n *run.Run) error { n.Status = run.Paused; return nil })
		return next, errors.Join(cause, err)
	}
	next, err := x.save(ctx, func(n *run.Run) error {
		n.Status, n.Error, n.Expires = run.Error, cause.Error(), expires(keep, x.rn.now())
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
	return x.notRun(ctx, r, true, nil, cause)
}

// current is the run as stored, with err.
func (x *execution) current(ctx context.Context, err error) (run.Run, error) {
	r, gerr := x.rn.Runs.Get(ctx, x.tenant, x.id)
	return r, errors.Join(err, gerr)
}
