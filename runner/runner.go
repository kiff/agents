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
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/usage"
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
	// FailedSwitching: the agent began switching versions before the run
	// started (kiff/tafs#107).
	FailedSwitching = "the agent is switching versions, so the run didn't start"
	// FailedNoAccess: the member who asked may no longer use the agent
	// (#221), or the account couldn't say whether they may.
	FailedNoAccess = "the person who asked may no longer use this agent, so the run stopped"
	// FailedHalted: the agent, or its tenant, was halted before the run
	// started or went on from a pause.
	FailedHalted = "the agent is halted, so the run didn't go on"
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
	// Sealer seals what a chat keeps of its reads for its next runs
	// (session.KeptRead), and opens it for them. Nil: nothing is kept.
	Sealer session.Sealer
	// Usage adds a counted run's cost to its period each time the run pauses
	// or ends (kiff/tafs#193). Nil: nothing is counted.
	Usage usage.Store
	// Access answers whether a member may use the agent (#221). A member's
	// run starts only if they may, and stops at its next checkpoint once they
	// may not; no answer is no. Runs no member asked for aren't checked. Nil:
	// nothing is checked.
	Access run.AccessFunc
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
	// Nothing of a halted agent, or of one whose tenant is halted, starts:
	// not even a trial, whose reads still reach a vendor.
	if err := rn.notHalted(ctx, s.Tenant, s.Agent, time.Time{}); err != nil {
		return run.Run{}, err
	}
	if err := rn.mayUse(ctx, s.Tenant, s.Agent, s.Requester); err != nil {
		return run.Run{}, err
	}
	trial := s.Trigger == run.ByTrial
	if !trial && (s.Version != 0 || s.Draft != "") {
		return run.Run{}, errors.New("runner: only a trial runs a version or a draft")
	}
	if !trial {
		// Nothing of an agent that is switching versions runs (kiff/tafs#107).
		if err := rn.notSwitching(ctx, s.Tenant, s.Agent); err != nil {
			return run.Run{}, err
		}
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
		// A requester's new session is theirs (#219).
		sess, err := rec.OpenOwned(ctx, s.Tenant, s.Agent, kind, s.Requester.Key())
		if err != nil {
			return run.Run{}, err
		}
		s.Session, opened = sess.ID, true
	} else {
		sess, _, err := rn.Sessions.Read(ctx, s.Tenant, s.Session)
		if err != nil {
			return run.Run{}, err
		}
		// Another agent's session, or another person's, is not found.
		if sess.Agent != s.Agent || !sess.OwnedBy(s.Requester.Key()) {
			return run.Run{}, session.ErrNotFound
		}
	}
	r := run.New(s.Tenant, s.Agent, s.Session, s.Trigger, s.Input, rn.now())
	r.Source = s.Source
	r.Requester = s.Requester
	r.Grant = s.Grant
	r.NewSession = opened
	r.Meter = s.Meter
	r.Notify = s.Notify
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

// notSwitching is registry.ErrSwitching while the agent is switching
// versions (kiff/tafs#107), and nil otherwise.
func (rn *Runner) notSwitching(ctx context.Context, tenant, agent string) error {
	sw, on, err := rn.Builder.Agents.Switching(ctx, tenant, agent)
	if err != nil {
		return fmt.Errorf("runner: reading whether the agent is switching versions: %w", err)
	}
	if on {
		return fmt.Errorf("%w (to version %d)", registry.ErrSwitching, sw.N)
	}
	return nil
}

// mayUse is run.ErrNoAccess unless the requester may use the agent (#221):
// a member, as the account answers; anyone else, as before. An answer that
// couldn't be had is no.
func (rn *Runner) mayUse(ctx context.Context, tenant, agent string, q run.Requester) error {
	if rn.Access == nil || q.Kind != run.ByMember {
		return nil
	}
	a, err := rn.Access(ctx, tenant, agent, q.ID)
	if err != nil {
		return fmt.Errorf("%w: the account couldn't say: %v", run.ErrNoAccess, err)
	}
	if !a.Use {
		return errDenied
	}
	return nil
}

// errDenied is the account's "no", as against no answer.
var errDenied = fmt.Errorf("%w: the account says no", run.ErrNoAccess)

// notHalted is registry.ErrHalted while the agent or its tenant is halted,
// or for a run created at or before the latest halt
// (created zero: none to check), so what came before a halt never runs, even
// once resumed; nil otherwise.
func (rn *Runner) notHalted(ctx context.Context, tenant, agent string, created time.Time) error {
	h, err := registry.Halts(ctx, rn.Builder.Agents, tenant, agent)
	if err != nil {
		return fmt.Errorf("runner: reading whether the agent is halted: %w", err)
	}
	if h.On {
		return registry.ErrHalted
	}
	if !created.IsZero() && h.Before(created) {
		return fmt.Errorf("%w: the run is from before its latest halt", registry.ErrHalted)
	}
	return nil
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
	// Checked once the run is claimed: a switch begins only while none of
	// the agent's runs is active, so either it saw this one, or this sees
	// the switch. A paused run goes back to waiting; a new one doesn't start.
	if r.Trigger != run.ByTrial {
		if err := rn.notSwitching(ctx, tenant, r.Agent); err != nil {
			return x.notRun(ctx, r, paused, nil, FailedSwitching, err)
		}
	}

	// Checked once the run is claimed too, as the switch is: a halt cancels
	// the agent's active runs once it is stored, so either it saw this one,
	// or this sees the halt. A new run ends CANCELLED, never started. A
	// paused one isn't collected: collecting an approved hold is what has
	// KIFF send it, and a halt holds here even when KIFF can't be asked. It
	// records each held call as not sent and ends at its first checkpoint,
	// before any model turn.
	if err := rn.notHalted(ctx, tenant, r.Agent, r.Created); err != nil {
		x.halted = paused
		if !paused {
			next, serr := x.saveStop(ctx, func(n *run.Run) error {
				n.Status, n.Cancel, n.Error, n.Expires = run.Cancelled, true, FailedHalted, expires(nil, rn.now())
				return nil
			})
			return next, errors.Join(err, serr)
		}
		if !r.Cancel {
			if r, err = x.save(ctx, func(n *run.Run) error { n.Cancel = true; return nil }); err != nil {
				return x.notRun(ctx, r, true, nil, "", err)
			}
		}
	}

	// A trial runs its own version, its tools marked so its writes are only
	// decided and its notes kept as they are (kiff/tafs#107); any other run
	// runs the live one.
	version := 0
	if r.Trigger == run.ByTrial {
		ctx = run.WithTrial(ctx, run.Trial{Draft: r.Draft, Run: r.ID})
		loop = run.WithTrial(loop, run.Trial{Draft: r.Draft, Run: r.ID})
		version = r.AgentVersion
	}
	// The member who asked must still be able to use the agent (#221): a run
	// of someone who lost it ends CANCELLED, never started; a paused one
	// stops at its first checkpoint, as a halted one does.
	if err := rn.mayUse(ctx, tenant, r.Agent, r.Requester); err != nil {
		x.noAccess = true
		if !paused {
			next, serr := x.saveStop(ctx, func(n *run.Run) error {
				n.Status, n.Cancel, n.Error, n.Expires = run.Cancelled, true, FailedNoAccess, expires(nil, rn.now())
				return nil
			})
			return next, errors.Join(err, serr)
		}
	}

	// Its tools know their run, and act for whoever asked: the memory tool
	// reads and writes that person's notes (#219).
	ctx = run.WithRef(ctx, run.Ref{Session: r.Session, Run: r.ID})
	loop = run.WithRef(loop, run.Ref{Session: r.Session, Run: r.ID})
	if r.Requester.Key() != "" {
		ctx = run.WithRequester(ctx, r.Requester)
		loop = run.WithRequester(loop, r.Requester)
	}
	if r.Grant != "" {
		ctx = run.WithGrant(ctx, r.Grant)
		loop = run.WithGrant(loop, r.Grant)
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
	p, err := rn.past(ctx, sess, events, r, d, a.Tools)
	if err != nil {
		return x.notRun(ctx, r, paused, d.Keep, FailedStart, fmt.Errorf("reading its history: %w", err))
	}
	a.Fast = fast(d, r.Trigger, events)
	rec, err := session.NewRecorderAt(rn.Sessions, d.Keep, rn.now)
	if err != nil {
		return x.notRun(ctx, r, paused, d.Keep, FailedStart, err)
	}
	rec.Sealing(rn.Sealer)

	if !paused {
		recording, err := rec.Start(ctx, sess, r.ID, r.Created)
		if err != nil {
			return x.notRun(ctx, r, false, d.Keep, FailedStart, fmt.Errorf("starting its recording: %w", err))
		}
		if len(p.fold) > 0 {
			a.Spent = rn.fold(loop, a, recording, &p)
		}
		history, _ := p.history()
		a.Preface = p.preface(loop, a.Tools, rn.now())
		// From the run's creation to its first model turn: the claim, the
		// build, the session's read and any cold start, on that turn's Usage.
		a.Started = rn.now().Sub(r.Created)
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
	// Summarised before KIFF is asked, so nothing slow comes between
	// collecting and recording.
	if len(p.fold) > 0 {
		a.Spent = rn.fold(loop, a, recording, &p)
	}
	var held []agent.ToolResultBody
	// Collecting an approved hold is what has KIFF send it: not for a halted
	// agent, nor for a member who may no longer use it (#221).
	switch {
	case x.halted:
		held = notCollected(r.Requirements, NotCollected)
	case x.noAccess:
		held = notCollected(r.Requirements, NotCollectedNoAccess)
	default:
		if held, err = rn.Collect(ctx, a, r.Requirements); err != nil {
			return x.repause(ctx, recording, r, fmt.Errorf("collecting its held calls: %w", err))
		}
	}
	for i := range held {
		if x.halted || x.noAccess {
			break // never asked of KIFF: not an outcome to explain
		}
		for _, q := range r.Requirements {
			if q.CallID == held[i].CallID {
				held[i].Result = HeldOutcome(q, held[i].Result)
			}
		}
	}
	history, _ := p.history()
	a.Preface = p.preface(loop, a.Tools, rn.now())
	out, msgs, err := agent.Continue(loop, a, recording, history, r, held, emit)
	out, err = x.more(loop, a, recording, msgs, out, err, emit)
	return x.stop(ctx, recording, d.Keep, out, err)
}

// NotCollected is a held call's result when the run's agent was halted before
// it was collected: nothing asked KIFF for it, so it
// wasn't sent.
const NotCollected = "The agent was halted before this call was collected. It was not sent."

// NotCollectedNoAccess is a held call's result when the member who asked
// may no longer use the agent (#221): it wasn't collected, so not sent.
const NotCollectedNoAccess = "The person who asked may no longer use this agent, so this call wasn't collected. It was not sent."

// notCollected is each requirement's result as text.
func notCollected(reqs []run.Requirement, text string) []agent.ToolResultBody {
	out := make([]agent.ToolResultBody, 0, len(reqs))
	for _, q := range reqs {
		out = append(out, agent.ToolResultBody{CallID: q.CallID, Tool: q.Tool, Result: tool.Result{Text: text, IsError: true}})
	}
	return out
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

// fold summarises the runs of p that no longer fit, after its summary, and
// records the new summary in rec, so later runs start from it. A summary
// that fails leaves those runs out, said so (DroppedNote). It returns what
// summarising cost.
func (rn *Runner) fold(ctx context.Context, a agent.Agent, rec *session.Recording, p *past) int64 {
	prior, runs := "", 0
	if p.summary != nil {
		prior, runs = p.summary.Text, p.summary.Runs
	}
	text, cost, err := agent.Summarize(ctx, a, rec, prior, flatten(p.fold))
	if err != nil {
		p.dropped, p.fold = true, nil
		return cost
	}
	s := session.SummaryBody{Text: text, Opening: p.opening, Through: p.fold[len(p.fold)-1].id, Runs: runs + len(p.fold)}
	// Not recorded, the summary still serves this run; the next one makes it again.
	_ = rec.Record(session.Summary, s)
	p.summary, p.fold = &s, nil
	return cost
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
	// halted: the agent was halted when this paused run was claimed, so its
	// held calls aren't collected, and it ends
	// CANCELLED saying so.
	halted bool
	// noAccess: the member who asked may no longer use the agent (#221), so
	// the run stopped at a checkpoint, saying so.
	noAccess bool
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
	// The member who asked may have lost the agent meanwhile (#221). A run
	// already going asks twice before it stops, so one passing failure to
	// answer doesn't end long work; a "no" stops it at once.
	if !x.noAccess {
		err := x.rn.mayUse(ctx, x.tenant, r.Agent, r.Requester)
		if err != nil && !errors.Is(err, errDenied) {
			err = x.rn.mayUse(ctx, x.tenant, r.Agent, r.Requester)
		}
		x.noAccess = err != nil
	}
	if x.noAccess {
		return true, nil
	}
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
		var before int64
		r, err := x.saveStop(ctx, func(n *run.Run) error {
			before = n.Cost
			n.Status, n.Stop, n.Output, n.Error = run.Paused, string(out.Stop), out.Text, ""
			n.Cost, n.Turns, n.Requirements, n.Paused = out.Cost, out.Turns, out.Requirements, out.Turn
			n.Retries, n.Retry = 0, time.Time{} // a new pause: the sweep's backoff starts again
			return nil
		})
		if err == nil {
			err = x.count(ctx, r, r.Cost-before)
		}
		return r, errors.Join(runErr, err) // a failed turn after the hold is still the caller's to log
	}
	if err := rec.End(ctx); err != nil {
		return x.current(ctx, errors.Join(runErr, fmt.Errorf("runner: ending the recording: %w", err)))
	}
	var before int64
	r, err := x.saveStop(ctx, func(n *run.Run) error {
		before = n.Cost
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
			switch {
			case x.halted:
				n.Error = FailedHalted
			case x.noAccess:
				n.Error = FailedNoAccess
			}
		default:
			n.Status, n.Stop, n.Output, n.Refusal = run.Completed, string(out.Stop), out.Text, out.Refusal
		}
		return nil
	})
	if err == nil {
		err = x.count(ctx, r, r.Cost-before)
	}
	return r, errors.Join(runErr, err)
}

// count adds what the run spent since it was last stored to its period, for
// AI fair use (kiff/tafs#193): once per pause or end, so a run continued after
// a pause is counted for each part, never twice. A run with no period isn't
// counted. A failed add is logged by the caller; the run is stored either way.
func (x *execution) count(ctx context.Context, r run.Run, spent int64) error {
	if r.Meter == "" || spent <= 0 || x.rn.Usage == nil {
		return nil
	}
	if err := x.rn.Usage.Add(ctx, r.Tenant, r.Meter, spent, x.rn.now().Add(server.UsageKeep)); err != nil {
		return fmt.Errorf("runner: counting the run's AI use: %w", err)
	}
	return nil
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

// fast says whether a run's turns go at the fast speed: its agent is set to,
// someone waits on it (an owner's message, a trial, an eval; never a
// schedule or an event), and its conversation began fast and stayed so. The
// provider's cache is per speed, so a conversation never changes speed.
func fast(d registry.Definition, trigger run.Trigger, events []session.Event) bool {
	if d.Speed != registry.SpeedFast {
		return false
	}
	switch trigger {
	case run.ByOwner, run.ByTrial, run.ByEval:
	default:
		return false
	}
	speed := session.SpeedOf(events)
	return speed == "" || speed == session.SpeedFast
}
