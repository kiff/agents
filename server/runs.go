package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/usage"
)

// MaxInput bounds what a run is asked, in bytes.
const MaxInput = 32_000

// followEvery is how often a stream looks for more of a run.
const followEvery = 500 * time.Millisecond

// Run listing limits.
const (
	defaultRuns = 20
	maxRuns     = 100
)

// cancelTries bounds how often a cancel re-reads a run that changed under it:
// a running run is saved at every checkpoint, so a cancel can meet one.
const cancelTries = 3

// Runner starts and executes runs. The server owns HTTP, SSE and who the
// caller is; the Runner owns how a run is created, claimed, run and stored.
type Runner interface {
	// Start creates a PENDING run for the tenant's agent: in Session (an
	// existing conversation) or in a new session when it is empty. It
	// returns the run as stored; session.ErrNotFound when Session isn't the
	// tenant's, or is another agent's (a session is one agent's thread).
	Start(ctx context.Context, s Start) (run.Run, error)
	// Execute claims the run and runs it to its next stop (answered, a cap,
	// paused on a hold, cancelled, failed), passing each event to emit, nil
	// in the background. It returns the run as stored at that stop. ctx
	// bounds the run, not the client: the server detaches it from the
	// request, keeping the invocation's deadline, so a client that goes away
	// doesn't end the run.
	//
	// A cancel can change the run between two of its saves: the server sets
	// Cancel with a compare-and-set. run.Save re-reads the run for the
	// claimant and writes its change onto it; if the run it returns has
	// Cancel set, the claimant ends it CANCELLED at that checkpoint. If KIFF
	// holds a call, the run pauses with Cancel set: run.Claim takes it again
	// once KIFF has resolved it, and it ends CANCELLED at its first checkpoint.
	Execute(ctx context.Context, tenant, id string, emit func(agent.Event)) (run.Run, error)
}

// Start is what a run is started with.
type Start struct {
	Tenant, Agent, Session string
	Trigger                run.Trigger
	// Source is what started it within its trigger (run.Run.Source).
	Source string
	// Requester is who asked (run.Run.Requester): a new session is theirs,
	// and an existing one must be theirs, or have no owner.
	Requester run.Requester
	Input     string
	// Version is the agent's version a trial runs (kiff/tafs#107); 0 runs
	// the live one. Draft is the KIFF draft its writes are decided against.
	// Only a trial (Trigger run.ByTrial) has either.
	Version int
	Draft   string
	// Meter is the billing period the run's cost counts toward for AI fair
	// use (kiff/tafs#193); empty, it isn't counted.
	Meter string
	// Notify is where the run is announced when it ends or pauses
	// (kiff/tafs#206); zero, nowhere.
	Notify run.Notify
	// ID is the run's ID, when the caller names it; empty, a new one. A
	// caller that may start the same run more than once (a schedule's firing,
	// delivered at least once) names it, and the second Start returns the run
	// as stored with run.ErrExists. Never taken from a request as is: an
	// event's run ID is derived from its key (EventRunID).
	ID string
}

// runView is a run as the API shows it: no paused turn (it holds what tools
// returned, and exists only to continue the run) and no claim.
type runView struct {
	ID        string        `json:"id"`
	Agent     string        `json:"agent"`
	Session   string        `json:"session"`
	Trigger   run.Trigger   `json:"trigger"`
	Source    string        `json:"source,omitempty"`
	Requester run.Requester `json:"requester,omitzero"`
	// NewSession is whether the run opened its session (#219); always
	// present, so a client can rely on it.
	NewSession      bool              `json:"new_session"`
	Status          run.Status        `json:"status"`
	Stop            string            `json:"stop,omitempty"`
	Refusal         string            `json:"refusal,omitempty"`
	Output          string            `json:"output,omitempty"`
	Error           string            `json:"error,omitempty"`
	Cost            int64             `json:"cost_micro_usd"`
	Turns           int               `json:"turns"`
	Requirements    []run.Requirement `json:"requirements,omitempty"`
	CancelRequested bool              `json:"cancel_requested,omitempty"`
	// Steering is what the owner wrote to the run while it went, each marked
	// once the run took it. One not taken when the run has ended is the
	// caller's to send as the next message.
	Steering []run.Steering `json:"steering,omitempty"`
	// AgentVersion is the agent's version the run ran, and Draft the KIFF
	// draft a trial's writes were decided against (kiff/tafs#107).
	AgentVersion int       `json:"agent_version,omitempty"`
	Draft        string    `json:"draft,omitempty"`
	Created      time.Time `json:"created"`
	Updated      time.Time `json:"updated"`
}

func viewRun(r run.Run) runView {
	return runView{ID: r.ID, Agent: r.Agent, Session: r.Session, Trigger: r.Trigger, Source: r.Source, Requester: r.Requester, NewSession: r.NewSession, Status: r.Status, Stop: r.Stop, Refusal: r.Refusal,
		Output: r.Output, Error: r.Error, Cost: r.Cost, Turns: r.Turns, Requirements: r.Requirements,
		CancelRequested: r.Cancel && !r.Status.Final(), Steering: r.Steering, AgentVersion: r.AgentVersion, Draft: r.Draft,
		Created: r.Created, Updated: r.Updated}
}

// agentRun is the tenant's run of the agent in the path; a run of another
// agent is not found.
func (s *Server) agentRun(ctx context.Context, c Caller, r *http.Request) (run.Run, error) {
	var rn run.Run
	var err error
	if ex := r.PathValue("exception"); ex != "" {
		// …/holds/{exception}/…: the run that paused on that held call.
		rn, err = s.Runs.ByHold(ctx, c.Tenant, ex)
	} else {
		rn, err = s.Runs.Get(ctx, c.Tenant, r.PathValue("run_id"))
	}
	if err != nil {
		return run.Run{}, err
	}
	if rn.Agent != r.PathValue("id") {
		return run.Run{}, run.ErrNotFound
	}
	return rn, nil
}

// ActiveScanned bounds how many of an agent's newest runs GET
// /agents/{id}/runs?active=true looks through for the ones not ended.
const ActiveScanned = 200

// listRuns lists an agent's runs, newest first, at most limit (20 by
// default, 100 at most), without their paused turn. With active=true, only
// the PENDING, RUNNING and PAUSED ones among its ActiveScanned newest: what
// it is doing and waiting on (kiff/tafs#206).
func (s *Server) listRuns(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	id := r.PathValue("id")
	if _, err := s.Agents.Get(ctx, c.Tenant, id); err != nil {
		return 0, nil, err
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			return 0, nil, bad("limit is 1 to 100")
		}
		limit = n
	}
	active := r.URL.Query().Get("active") == "true"
	scan := limit
	if active {
		scan = ActiveScanned
	}
	runs, err := s.Runs.List(ctx, c.Tenant, id, scan)
	if err != nil {
		return 0, nil, err
	}
	out := []runView{}
	for _, rn := range runs {
		if active && (rn.Status.Final()) {
			continue
		}
		if out = append(out, viewRun(rn)); len(out) == limit {
			break
		}
	}
	return http.StatusOK, map[string]any{"runs": out}, nil
}

func (s *Server) getRun(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	rn, err := s.agentRun(ctx, c, r)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, viewRun(rn), nil
}

// cancelRun asks a run to stop. A PENDING run is cancelled now; a RUNNING
// one stops at its next checkpoint. A PAUSED run's held calls are withdrawn
// in KIFF first (cancelPaused). A run saved meanwhile (a running run's
// checkpoint, an answer recorded) is read again, so the caller doesn't get a
// conflict it didn't cause.
func (s *Server) cancelRun(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	// requester, if the body names one (#219): a run another requester asked
	// for isn't theirs to stop.
	var body struct {
		Requester *run.Requester `json:"requester,omitempty"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &body); err != nil {
			return 0, nil, err
		}
	}
	q, err := requesterOf(body.Requester)
	if err != nil {
		return 0, nil, err
	}
	rn, err := s.agentRun(ctx, c, r)
	if err != nil {
		return 0, nil, err
	}
	if !theirs(rn, q) {
		return 0, nil, run.ErrNotFound // another person's run
	}
	next, err := s.cancel(ctx, rn)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, viewRun(next), nil
}

// requesterOf is a body's requester, checked; none when it names none.
func requesterOf(q *run.Requester) (run.Requester, error) {
	if q == nil {
		return run.Requester{}, nil
	}
	if !q.Valid() {
		return run.Requester{}, bad("requester is {kind: member, door, manager or agent; id}")
	}
	return *q, nil
}

// theirs reports whether q may act on rn (#219): a caller naming no one, a
// run no one asked for, or q's own.
func theirs(rn run.Run, q run.Requester) bool {
	return q.Key() == "" || rn.Requester.Key() == "" || rn.Requester == q
}

// cancel is cancelRun for rn, as read: a run saved meanwhile is read again,
// up to cancelTries times.
func (s *Server) cancel(ctx context.Context, rn run.Run) (run.Run, error) {
	for try := 1; ; try++ {
		if try > 1 {
			var err error
			if rn, err = s.Runs.Get(ctx, rn.Tenant, rn.ID); err != nil {
				return run.Run{}, err
			}
		}
		switch rn.Status {
		case run.Paused:
			next, err := s.cancelPaused(ctx, rn)
			if errors.Is(err, run.ErrConflict) && try < cancelTries {
				continue
			}
			return next, err
		case run.Completed, run.Error, run.Cancelled:
			return rn, conflict("the run has ended")
		}
		var expires time.Time
		if rn.Status == run.Pending {
			var err error
			if expires, err = s.endedExpires(ctx, rn); err != nil {
				return run.Run{}, err
			}
		}
		next, err := run.Change(ctx, s.Runs, rn, s.now(), func(n *run.Run) error {
			if n.Status == run.Pending {
				n.Status, n.Expires = run.Cancelled, expires
			}
			n.Cancel = true
			return nil
		})
		if errors.Is(err, run.ErrConflict) && try < cancelTries {
			continue
		}
		return next, err
	}
}

// steerBody is what POST /agents/{id}/runs/{run_id}/messages takes.
type steerBody struct {
	Text string `json:"text"`
	// Requester is who wrote the message (#219): it counts toward their
	// flood guard, and a run another person asked for isn't theirs to steer.
	Requester *run.Requester `json:"requester,omitempty"`
}

// steerRun gives a run that hasn't ended a message the owner wrote while it
// goes (#102): it is taken at the run's next checkpoint, before a model
// turn, and streamed then as a delta (owner_message). It answers 202 with the
// run. A run that has ended, or was asked to stop, is a conflict: the caller
// starts a new run with the message, as for any message. So is one that has
// taken run.MaxSteering messages. With a requester, a run another requester
// asked for is not found, and the message counts toward their flood guard.
func (s *Server) steerRun(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	var body steerBody
	if err := decode(r, &body); err != nil {
		return 0, nil, err
	}
	if strings.TrimSpace(body.Text) == "" || len(body.Text) > MaxInput {
		return 0, nil, bad("the text is 1 to %d bytes", MaxInput)
	}
	q, err := requesterOf(body.Requester)
	if err != nil {
		return 0, nil, err
	}
	rn, err := s.agentRun(ctx, c, r)
	if err != nil {
		return 0, nil, err
	}
	if !theirs(rn, q) {
		return 0, nil, run.ErrNotFound // another person's run
	}
	if q.Kind == run.ByMember {
		if err := s.mayRead(ctx, c.Tenant, rn.Agent, q.Key()); err != nil {
			return 0, nil, err
		}
	}
	if q.Key() != "" {
		if err := s.flood(ctx, c.Tenant, rn.Agent, q); err != nil {
			return 0, nil, err
		}
	}
	next, err := run.SteerBy(ctx, s.Runs, c.Tenant, rn.ID, body.Text, q, s.now())
	switch {
	case errors.Is(err, run.ErrTransition):
		return 0, nil, conflict("the run has ended, or was asked to stop: send the message as a new run")
	case errors.Is(err, run.ErrFull):
		return 0, nil, conflict("the run holds as many messages as it takes while it goes: wait for it to end, or stop it")
	case err != nil:
		return 0, nil, err
	}
	return http.StatusAccepted, viewRun(next), nil
}

// cancelPaused records the cancel on a paused run first, then withdraws
// every call KIFF holds for it, so an answer that comes later sends nothing.
// The cancel comes first because each withdrawal makes KIFF announce the
// hold invalidated, and the alert may continue the run before this returns:
// a paused run with Cancel set can only end CANCELLED. What became of each
// call is then recorded (the alerts record the same), and once the run is
// resolved it is handed to Background: it is claimed, collects the outcomes
// (a withdrawn call comes back refused; one KIFF had already sent, its
// result) and ends CANCELLED at its first checkpoint. If Background fails,
// the sweep continues it. A withdrawal that fails leaves the cancel
// recorded; repeating it withdraws the rest.
func (s *Server) cancelPaused(ctx context.Context, rn run.Run) (run.Run, error) {
	if s.Withdraw == nil {
		return run.Run{}, conflict("a paused run can't be cancelled here: nothing withdraws the calls KIFF holds")
	}
	for _, q := range rn.Requirements {
		if q.Exception == "" {
			return run.Run{}, conflict("a paused run can't be cancelled: KIFF holds a call it gave no ID to withdraw it by")
		}
	}
	if !rn.Cancel {
		var err error
		if rn, err = run.Change(ctx, s.Runs, rn, s.now(), func(n *run.Run) error { n.Cancel = true; return nil }); err != nil {
			return run.Run{}, err
		}
	}
	for _, q := range rn.Requirements {
		status, err := s.Withdraw(ctx, rn, q.Exception)
		if err != nil {
			return run.Run{}, fmt.Errorf("withdrawing held call %s: %w", q.CallID, err)
		}
		if status == "" {
			continue // KIFF holds nothing for this key to withdraw: its answer is KIFF's to send
		}
		if _, err := run.Answer(ctx, s.Runs, rn.Tenant, q.Exception, status, s.now()); err != nil {
			return run.Run{}, fmt.Errorf("recording held call %s withdrawn: %w", q.CallID, err)
		}
	}
	next, err := s.Runs.Get(ctx, rn.Tenant, rn.ID)
	if err != nil {
		return run.Run{}, err
	}
	if s.Background != nil && next.Status == run.Paused && resolved(next, s.now()) {
		if err := s.Background(context.WithoutCancel(ctx), next.Tenant, next.ID); err != nil && s.Log != nil {
			s.Log.ErrorContext(ctx, "continuing a cancelled paused run; the sweep will", "run", next.ID, "err", err)
		}
	}
	return next, nil
}

// askKIFF records what KIFF answered on rn's held calls that no alert has
// reported yet, as the alert would (run.Answer), and returns the run as
// stored. The answer comes from KIFF, read with the agent's own key, never
// from the caller: a continue only asks sooner (kiff/tafs#206). A run still
// paused on a hold KIFF hasn't answered stays as it is.
func (s *Server) askKIFF(ctx context.Context, rn run.Run) (run.Run, error) {
	if s.Answers == nil {
		return rn, nil
	}
	answers, err := s.Answers(ctx, rn)
	if err != nil {
		if s.Log != nil {
			s.Log.ErrorContext(ctx, "asking KIFF for held calls' answers", "run", rn.ID, "err", err)
		}
		return run.Run{}, unavailable("KIFF couldn't be asked for the held calls' answers: try again, or wait for its alert")
	}
	asked := false
	for _, q := range rn.Requirements {
		a := answers[q.Exception]
		if q.Exception == "" || a == "" || !q.Answered.IsZero() {
			continue
		}
		if _, err := run.Answer(ctx, s.Runs, rn.Tenant, q.Exception, a, s.now()); err != nil {
			return run.Run{}, fmt.Errorf("recording KIFF's answer on held call %s: %w", q.CallID, err)
		}
		asked = true
	}
	if !asked {
		return rn, nil
	}
	return s.Runs.Get(ctx, rn.Tenant, rn.ID)
}

// resolved reports whether every requirement of r is resolved.
func resolved(r run.Run, now time.Time) bool {
	for _, q := range r.Requirements {
		if !q.Resolved(now) {
			return false
		}
	}
	return true
}

// endedExpires is when a run the server ends may go: its agent's keep rules,
// as the Recorder applies them to its session (zero: kept until deleted).
// Every run the server ends gets it, so none outlives what the rules keep.
func (s *Server) endedExpires(ctx context.Context, r run.Run) (time.Time, error) {
	d, err := s.Agents.Get(ctx, r.Tenant, r.Agent)
	if err != nil {
		return time.Time{}, err
	}
	if keep := d.Keep.SessionRetention(); keep > 0 {
		return s.now().Add(keep), nil
	}
	return time.Time{}, nil
}

// startBody is what POST /agents/{id}/runs takes.
type startBody struct {
	Input      string `json:"input"`
	SessionID  string `json:"session_id,omitempty"`
	Background bool   `json:"background,omitempty"`
	// AI counts the run's cost toward its account's AI fair use, and refuses
	// to start it past the bound (kiff/tafs#193). Nil: not counted.
	AI *aiBody `json:"ai,omitempty"`
	// Notify announces the run when it ends or pauses, at a URL the server
	// allows (Server.NotifyURLs), with the caller's ref (kiff/tafs#206).
	Notify *run.Notify `json:"notify,omitempty"`
	// Requester is who asks (#219): a new session is theirs, and a named
	// one must be theirs or have no owner.
	Requester *run.Requester `json:"requester,omitempty"`
	// Chat "live" goes on in the requester's live chat: their latest chat,
	// if its last message is within Server.LiveChat, else a new one. It
	// needs a requester and no session_id. Empty: session_id, or a new one.
	Chat string `json:"chat,omitempty"`
}

// DefaultLiveChat is Server.LiveChat's default: a chat the requester wrote
// in within the last half hour is the one their next message goes on in.
const DefaultLiveChat = 30 * time.Minute

// The flood guard's defaults (Server.FloodMessages, FloodWindow): ten
// messages in ten minutes, per requester and agent.
const (
	DefaultFloodMessages = 10
	DefaultFloodWindow   = 10 * time.Minute
)

// floodScan is how many of the agent's latest runs the flood guard reads. On
// an agent busier than that, it counts only what it reads, and lets through
// rather than refuses; so it does for a message steered into a run older
// than those.
const floodScan = 100

// flood refuses a requester's message past their flood guard: their own
// messages to the agent in the window, never anyone else's (#219). A
// message is a run they started, or one they steered into a run. A door's
// sender is held back by the door's own caps, before the message gets
// here: refusing it here would only have the delivery tried again.
func (s *Server) flood(ctx context.Context, tenant, agent string, q run.Requester) error {
	if q.Kind == run.ByDoor {
		return nil
	}
	limit, window := s.FloodMessages, s.FloodWindow
	if limit <= 0 {
		limit = DefaultFloodMessages
	}
	if window <= 0 {
		window = DefaultFloodWindow
	}
	runs, err := s.Runs.List(ctx, tenant, agent, floodScan)
	if err != nil {
		return err
	}
	since, n := s.now().Add(-window), 0
	for _, r := range runs {
		// An older run may hold a message steered in the window: read on.
		if r.Trigger == run.ByOwner && r.Requester == q && !r.Created.Before(since) {
			n++
		}
		for _, m := range r.Steering {
			if m.Requester == q && !m.At.Before(since) {
				n++
			}
		}
	}
	if n >= limit {
		return floodError{n: limit, window: window}
	}
	return nil
}

// liveChats is how many of a requester's latest sessions liveChat reads to
// find their latest chat among their trials and the sessions with no owner.
const liveChats = 20

// liveChat is the requester's live chat with the agent (#219): their
// latest chat, if its last message is recent enough; else empty, and the
// message starts a new one. Only a chat the requester owns is theirs: never
// another person's, nor one from before owners.
func (s *Server) liveChat(ctx context.Context, tenant, agent string, q run.Requester) (string, error) {
	list, err := s.Sessions.ListOwned(ctx, tenant, agent, q.Key(), liveChats)
	if err != nil {
		return "", err
	}
	window := s.LiveChat
	if window <= 0 {
		window = DefaultLiveChat
	}
	for _, sess := range list {
		if sess.Owner != q.Key() || sess.Kind != session.KindChat {
			continue
		}
		if !sess.Archived && s.now().Sub(sess.Updated) < window {
			return sess.ID, nil
		}
		return "", nil // their latest chat has gone quiet, or they put it away
	}
	return "", nil
}

// notify checks where a run asks to be announced (kiff/tafs#206): a URL the
// server allows, exactly, and a ref no one guesses. Nil is nowhere.
func (s *Server) notify(n *run.Notify) (run.Notify, error) {
	if n == nil {
		return run.Notify{}, nil
	}
	if !slices.Contains(s.NotifyURLs, n.URL) {
		return run.Notify{}, bad("notify.url is not one this server announces runs to")
	}
	if len(n.Ref) < MinRef || len(n.Ref) > MaxRef {
		return run.Notify{}, bad("notify.ref is %d to %d bytes", MinRef, MaxRef)
	}
	return *n, nil
}

// MaxRef bounds a notify ref; MinRef keeps it a value no one guesses.
const (
	MinRef = 16
	MaxRef = 128
)

// aiBody names the billing period a chat counts toward and its bound, in
// micro US dollars; 0 is no bound, the run is only counted.
type aiBody struct {
	Period string `json:"period"`
	Bound  int64  `json:"bound_micro_usd"`
}

// MaxPeriod bounds a billing period's name.
const MaxPeriod = 64

// The stream a run's start and resume share, as server-sent events:
//
//	event: run    the run as stored, once, first (start and continue only)
//	event: event  one recorded session event of the run, its id the event's
//	              Seq: the cursor resume?after= takes
//	event: delta  what the agent does as it does it (an agent.Event): text as
//	              it is written, tool calls as they are made, an owner's
//	              message as the run takes it (POST …/messages). No id: it is for
//	              showing, and the recorded events carry what is kept. Start
//	              and continue only, in the foreground
//	event: end    the run as stored at its stop; its status is empty when
//	              the run couldn't be read back, and the client reads it
//
// A client that loses the stream resumes after the last id it saw: the run's
// recorded events come again from there, none twice.

// startRun starts a run of the agent: streamed as SSE, or in the background,
// answering 202 once it is stored PENDING.
func (s *Server) startRun(w http.ResponseWriter, r *http.Request) {
	c, err := s.caller(w, r)
	if err != nil {
		return
	}
	ctx := r.Context()
	var body startBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Input == "" || len(body.Input) > MaxInput {
		s.fail(w, r, bad("the input is 1 to %d bytes", MaxInput))
		return
	}
	if body.Background && s.Background == nil {
		s.fail(w, r, unavailable("this server doesn't run runs in the background"))
		return
	}
	var q run.Requester
	if body.Requester != nil {
		if q = *body.Requester; !q.Valid() {
			s.fail(w, r, bad("requester is {kind: member, door, manager or agent; id}"))
			return
		}
	}
	switch body.Chat {
	case "":
	case "live":
		if q.Key() == "" || body.SessionID != "" {
			s.fail(w, r, bad(`chat "live" needs a requester, and no session_id`))
			return
		}
	default:
		s.fail(w, r, bad(`chat is "live", or left out`))
		return
	}
	id := r.PathValue("id")
	if _, err := s.Agents.Get(ctx, c.Tenant, id); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.SessionID != "" {
		// A session is one agent's thread, and an owned one its owner's:
		// another agent's, or another person's, is not found.
		sess, _, err := s.Sessions.Read(ctx, c.Tenant, body.SessionID)
		if err == nil && (sess.Agent != id || !sess.OwnedBy(q.Key())) {
			err = session.ErrNotFound
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if q.Key() != "" {
		if err := s.flood(ctx, c.Tenant, id, q); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if body.Chat == "live" {
		live, err := s.liveChat(ctx, c.Tenant, id, q)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		body.SessionID = live
	}
	st := Start{Tenant: c.Tenant, Agent: id, Session: body.SessionID, Trigger: run.ByOwner, Input: body.Input, Requester: q}
	if body.AI != nil {
		if err := s.fairUse(ctx, c.Tenant, *body.AI); err != nil {
			s.fail(w, r, err)
			return
		}
		st.Meter = body.AI.Period
	}
	if st.Notify, err = s.notify(body.Notify); err != nil {
		s.fail(w, r, err)
		return
	}
	rn, err := s.start(ctx, st)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Background {
		if err := s.toBackground(ctx, rn); err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusAccepted, viewRun(rn))
		return
	}
	s.execute(w, r, c, rn, 0)
}

// continueBody is what POST /agents/{id}/runs/{run_id}/continue takes, if
// anything.
type continueBody struct {
	Background bool `json:"background,omitempty"`
}

// continueRun continues a PAUSED run whose held calls KIFF has resolved:
// streamed as SSE, or in the background, answering 202. Any other run is a
// conflict: one not paused (it went on, or ended; read it), or one that
// still waits for KIFF. The run is claimed once, so a continue that races an
// alert's or the sweep's executes it once, and the other gets the run as
// stored.
func (s *Server) continueRun(w http.ResponseWriter, r *http.Request) {
	c, err := s.caller(w, r)
	if err != nil {
		return
	}
	ctx := r.Context()
	var body continueBody
	if r.ContentLength != 0 {
		if err := decode(r, &body); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if body.Background && s.Background == nil {
		s.fail(w, r, unavailable("this server doesn't run runs in the background"))
		return
	}
	rn, err := s.agentRun(ctx, c, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if rn.Status != run.Paused {
		s.fail(w, r, conflict("the run isn't paused: read it"))
		return
	}
	if !resolved(rn, s.now()) {
		if rn, err = s.askKIFF(ctx, rn); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if !resolved(rn, s.now()) {
		s.fail(w, r, conflict("the run still waits for KIFF to resolve its held calls"))
		return
	}
	// Continued, the run is active again. The sweep would continue it too,
	// uncapped: a 429 here only asks the caller not to add one more now.
	if err := s.room(ctx, c.Tenant); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Background {
		// A failed hand-off leaves the run paused; the sweep continues it.
		if err := s.Background(ctx, rn.Tenant, rn.ID); err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusAccepted, viewRun(rn))
		return
	}
	// The stream carries what the run records from here on: what it
	// recorded before it paused, the caller has (resume?after=0 replays it).
	st, err := s.Sessions.State(ctx, c.Tenant, rn.Session, rn.ID)
	if err != nil && !errors.Is(err, session.ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	s.execute(w, r, c, rn, st.Seq)
}

// execute runs rn in this invocation, streamed to the caller as SSE: the run
// first, then what it does and the events it records after Seq after, then
// where it stopped.
func (s *Server) execute(w http.ResponseWriter, r *http.Request, c Caller, rn run.Run, after int64) {
	ctx := r.Context()
	sse := newStream(w)
	sse.send("run", viewRun(rn))
	f := &follower{s: s, sse: sse, tenant: c.Tenant, rn: rn, after: after}
	// The run's recorded events follow while it executes; the follower is
	// this request's, and ends before the stream does.
	done := make(chan struct{})
	followed := make(chan struct{})
	go func() {
		defer close(followed)
		tick := time.NewTicker(followEvery)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				// The client went away: nothing more to send it. The run
				// carries on, and the client can resume from its cursor.
				return
			case <-tick.C:
				if sse.gone() {
					return // a write failed: the client went away
				}
				if err := f.catchUp(ctx); err != nil {
					s.logError(r, "following a run", err)
				}
			}
		}
	}()
	// The run is the invocation's, not the client's: a client that goes away
	// doesn't stop it. Its context keeps the invocation's deadline, which the
	// Runner's lease needs.
	runCtx := context.WithoutCancel(ctx)
	if dl, ok := ctx.Deadline(); ok {
		var cancelRun context.CancelFunc
		runCtx, cancelRun = context.WithDeadline(runCtx, dl)
		defer cancelRun()
	}
	end, err := s.Runner.Execute(runCtx, c.Tenant, rn.ID, func(e agent.Event) { sse.send("delta", e) })
	close(done)
	<-followed
	switch {
	case errors.Is(err, run.ErrConflict), errors.Is(err, run.ErrTransition), errors.Is(err, run.ErrNotReady):
		// Another invocation claimed the run first (an alert's, the
		// sweep's): expected, and the end below is the run as stored.
		if s.Log != nil {
			s.Log.InfoContext(ctx, "the run was claimed elsewhere", "run", rn.ID, "err", err)
		}
	case err != nil:
		s.logError(r, "run failed", err)
	}
	if err != nil {
		if got, gerr := s.Runs.Get(context.WithoutCancel(ctx), c.Tenant, rn.ID); gerr == nil {
			end = got
		} else {
			end.Status = "" // unknown: the client reads the run
		}
	}
	if ctx.Err() != nil || sse.gone() {
		return // nobody is left to read the rest
	}
	if err := f.catchUp(ctx); err != nil { // what was recorded since the last look
		s.logError(r, "following a run", err)
	}
	sse.send("end", viewRun(end))
}

// resumeRun picks a run's stream back up: its recorded events after the index
// the client last saw, then the rest as the run goes on, until it stops.
func (s *Server) resumeRun(w http.ResponseWriter, r *http.Request) {
	c, err := s.caller(w, r)
	if err != nil {
		return
	}
	ctx := r.Context()
	after := int64(0)
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			s.fail(w, r, bad("after is an event index, 0 or more"))
			return
		}
		after = n
	}
	rn, err := s.agentRun(ctx, c, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	f := &follower{s: s, sse: newStream(w), tenant: c.Tenant, rn: rn, after: after}
	tick := time.NewTicker(followEvery)
	defer tick.Stop()
	for {
		if err := f.catchUp(ctx); err != nil {
			s.logError(r, "resume", err)
			return
		}
		if rn.Status.Final() || rn.Status == run.Paused {
			f.sse.send("end", viewRun(rn))
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if rn, err = s.Runs.Get(ctx, c.Tenant, rn.ID); err != nil {
			s.logError(r, "resume", err)
			return
		}
	}
}

// follower sends a run's recorded events as they are recorded, each once.
type follower struct {
	s      *Server
	sse    *stream
	tenant string
	rn     run.Run
	after  int64 // the last Seq sent
}

// catchUp sends what the run recorded since the last Seq sent. It reads the
// events only when the run's Seq has moved, so looking is one cheap read.
func (f *follower) catchUp(ctx context.Context) error {
	st, err := f.s.Sessions.State(ctx, f.tenant, f.rn.Session, f.rn.ID)
	if errors.Is(err, session.ErrNotFound) {
		return nil // nothing recorded yet
	}
	if err != nil || st.Seq <= f.after {
		return err
	}
	_, events, err := f.s.Sessions.ReadRun(ctx, f.tenant, f.rn.Session, f.rn.ID)
	if err != nil {
		return err
	}
	for _, e := range events {
		if e.Seq > f.after {
			if e.Kind != session.KeptRead { // only the runner reads it
				f.sse.event("event", strconv.FormatInt(e.Seq, 10), e)
			}
			f.after = e.Seq
		}
	}
	return nil
}

// caller verifies the request's caller for a route that writes its own
// response, writing the refusal if it isn't.
func (s *Server) caller(w http.ResponseWriter, r *http.Request) (Caller, error) {
	c, err := s.Auth(r)
	if err == nil && c.Tenant == "" {
		err = ErrUnauthorized
	}
	if err != nil {
		err = fmt.Errorf("%w: %w", ErrUnauthorized, err)
		s.fail(w, r, err)
		return Caller{}, err
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	return c, nil
}

func (s *Server) logError(r *http.Request, msg string, err error) {
	if s.Log != nil {
		s.Log.ErrorContext(r.Context(), msg, "method", r.Method, "path", r.URL.Path, "err", err)
	}
}

// stream writes server-sent events. The run's own goroutines and the
// follower write to it, so each event is written whole, one at a time. It
// remembers a failed write: the client went away, on any server, even where
// the request's context isn't cancelled for it (a Lambda function URL).
type stream struct {
	mu     sync.Mutex
	w      http.ResponseWriter
	f      http.Flusher
	broken bool
}

// gone reports whether a write failed: nobody is left to read.
func (s *stream) gone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.broken
}

func newStream(w http.ResponseWriter) *stream {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	return &stream{w: w, f: f}
}

func (s *stream) send(name string, v any) { s.event(name, "", v) }

// event writes one event: its name, its id when given, and v as one line of
// JSON (JSON has no raw newlines, so it is always one data line).
func (s *stream) event(name, id string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(map[string]string{"error": "the event could not be written"})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken {
		return
	}
	var werr error // the write's, apart from the encoding's: an event that
	// couldn't be encoded is still sent, as its fallback
	if id != "" {
		_, werr = fmt.Fprintf(s.w, "id: %s\n", id)
	}
	if werr == nil {
		_, werr = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, b)
	}
	if werr != nil {
		s.broken = true
		return
	}
	if s.f != nil {
		s.f.Flush()
	}
}

// toBackground has another invocation execute rn. If it can't, nothing will
// claim the run: it is ended now, ERROR, rather than left to the sweep.
func (s *Server) toBackground(ctx context.Context, rn run.Run) error {
	err := s.Background(ctx, rn.Tenant, rn.ID)
	if err == nil {
		return nil
	}
	expires, eerr := s.endedExpires(ctx, rn)
	if _, cerr := run.Change(ctx, s.Runs, rn, s.now(), func(n *run.Run) error {
		n.Status, n.Error, n.Expires = run.Error, "it could not be started in the background", expires
		return nil
	}); cerr != nil || eerr != nil {
		err = errors.Join(err, eerr, cerr)
	}
	return err
}

// triggerSchedule fires a schedule now, by hand: an ordinary run of its agent
// with its input, tagged with the schedule like any firing, executed in the
// background. It answers 202 with the run. The schedule needn't be on.
func (s *Server) triggerSchedule(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	if s.Background == nil {
		return 0, nil, unavailable("this server doesn't run runs in the background")
	}
	id := r.PathValue("id")
	sc, err := s.Schedules.Store.Get(ctx, c.Tenant, id)
	if err != nil {
		return 0, nil, err
	}
	rn, err := s.start(ctx, Start{Tenant: c.Tenant, Agent: sc.Agent, Trigger: run.BySchedule, Source: id, Input: sc.Input})
	if err != nil {
		return 0, nil, err
	}
	if err := s.toBackground(ctx, rn); err != nil {
		return 0, nil, err
	}
	return http.StatusAccepted, viewRun(rn), nil
}

// Event limits: an event's key and its source's name.
const (
	MaxEventKey    = 256
	MaxEventSource = 64
)

// eventBody is what POST /agents/{id}/events takes: work that came in for
// the agent, as the caller verified it.
type eventBody struct {
	// Key names the event at its origin (Stripe's event ID, an e-invoice's
	// number): one key starts one run, however often it is delivered.
	Key string `json:"key"`
	// Source is where it came from ("stripe", "einvoice"): the run's Source.
	Source string `json:"source"`
	// Input is what the agent is asked to do with it.
	Input string `json:"input"`
}

// EventRunID is the ID of the run an event starts: one per tenant, agent and
// event key.
func EventRunID(tenant, agent, key string) string {
	h := sha256.Sum256(fmt.Appendf(nil, "event|%d:%s|%d:%s|%d:%s", len(tenant), tenant, len(agent), agent, len(key), key))
	return hex.EncodeToString(h[:16])
}

// startEvent starts the agent's run for work that came in, in the
// background: 202 with the run. The same event delivered again starts
// nothing: 200 with the run it started, or 202 again if that run is still
// PENDING and is handed on once more.
func (s *Server) startEvent(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	if s.Background == nil {
		return 0, nil, unavailable("this server doesn't run runs in the background")
	}
	var body eventBody
	if err := decode(r, &body); err != nil {
		return 0, nil, err
	}
	switch {
	case body.Key == "" || len(body.Key) > MaxEventKey:
		return 0, nil, bad("the key is 1 to %d bytes", MaxEventKey)
	case !sourceName(body.Source):
		return 0, nil, bad("the source is 1 to %d of a-z, 0-9, '.', '_' and '-'", MaxEventSource)
	case body.Input == "" || len(body.Input) > MaxInput:
		return 0, nil, bad("the input is 1 to %d bytes", MaxInput)
	}
	id := r.PathValue("id")
	if _, err := s.Agents.Get(ctx, c.Tenant, id); err != nil {
		return 0, nil, err
	}
	rn, err := s.start(ctx, Start{Tenant: c.Tenant, Agent: id, Trigger: run.ByEvent, Source: body.Source,
		Input: body.Input, ID: EventRunID(c.Tenant, id, body.Key)})
	switch {
	case errors.Is(err, run.ErrExists) && rn.Status != run.Pending:
		return http.StatusOK, viewRun(rn), nil // delivered again: the run it started
	case err != nil && !errors.Is(err, run.ErrExists):
		return 0, nil, err
	}
	// A new run, or one delivered again still PENDING (its hand-off may have
	// failed): hand it on. It is claimed once, so a second hand-off is
	// harmless. If this one fails, the run stays PENDING for a redelivery;
	// the sweep ends it if none comes.
	if err := s.Background(ctx, rn.Tenant, rn.ID); err != nil {
		return 0, nil, err
	}
	return http.StatusAccepted, viewRun(rn), nil
}

// start is how every route starts a run: through the tenant's cap
// (MaxActive). A named run already stored is returned with run.ErrExists, as
// Runner.Start would, before the cap: a delivery again of an event already
// started gets its run, cap or not. TestEveryStartIsCapped keeps routes from
// calling Runner.Start themselves.
func (s *Server) start(ctx context.Context, st Start) (run.Run, error) {
	if st.ID != "" {
		rn, err := s.Runs.Get(ctx, st.Tenant, st.ID)
		switch {
		case err == nil:
			return rn, run.ErrExists
		case !errors.Is(err, run.ErrNotFound):
			return run.Run{}, err
		}
	}
	if err := s.room(ctx, st.Tenant); err != nil {
		return run.Run{}, err
	}
	return s.Runner.Start(ctx, st)
}

// room refuses one more active run for tenant at its cap (MaxActive).
func (s *Server) room(ctx context.Context, tenant string) error {
	if s.MaxActive <= 0 {
		return nil
	}
	n, err := s.Runs.CountActive(ctx, tenant)
	if err != nil {
		return err
	}
	if n >= s.MaxActive {
		return tooManyError{n: n, max: s.MaxActive}
	}
	return nil
}

// sourceName reports whether name is an event source's name.
func sourceName(name string) bool {
	if name == "" || len(name) > MaxEventSource {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// scheduleRuns lists the runs one of the tenant's schedules started, newest
// first.
func (s *Server) scheduleRuns(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	id := r.PathValue("id")
	if _, err := s.Schedules.Store.Get(ctx, c.Tenant, id); err != nil {
		return 0, nil, err
	}
	limit := defaultRuns
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxRuns {
			return 0, nil, bad("limit is 1 to %d", maxRuns)
		}
		limit = n
	}
	list, err := s.Runs.ListSource(ctx, c.Tenant, run.BySchedule, id, limit)
	if err != nil {
		return 0, nil, err
	}
	out := make([]runView, len(list))
	for i, rn := range list {
		out[i] = viewRun(rn)
	}
	return http.StatusOK, map[string]any{"runs": out}, nil
}

// ErrAIFairUse refuses a chat past its account's AI fair use for the period,
// until the owner says go on (kiff/tafs#193).
var ErrAIFairUse = errors.New("ai_fair_use: this period's AI use is past its bound; the owner says go on in KIFF to continue")

// fairUse refuses a chat whose period is past its bound. A run already going
// finishes: only starting is refused. The owner's go on is KIFF's: each one
// raises the bound tafs cloud sends.
func (s *Server) fairUse(ctx context.Context, tenant string, ai aiBody) error {
	if !sourceName(ai.Period) || len(ai.Period) > MaxPeriod || ai.Bound < 0 {
		return bad("ai.period is 1 to %d of a-z, 0-9, '.', '_' or '-', and ai.bound_micro_usd isn't negative", MaxPeriod)
	}
	if s.Usage == nil {
		return unavailable("this server doesn't count AI use")
	}
	p, err := s.Usage.Get(ctx, tenant, ai.Period)
	if err != nil {
		return err
	}
	if !usage.Allowed(p, ai.Bound) {
		return ErrAIFairUse
	}
	return nil
}

// getAIUsage answers the period's spend.
func (s *Server) getAIUsage(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	period := r.URL.Query().Get("period")
	if !sourceName(period) || len(period) > MaxPeriod {
		return 0, nil, bad("period is 1 to %d of a-z, 0-9, '.', '_' or '-'", MaxPeriod)
	}
	if s.Usage == nil {
		return 0, nil, unavailable("this server doesn't count AI use")
	}
	p, err := s.Usage.Get(ctx, c.Tenant, period)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, p, nil
}

// UsageKeep is how long a period's spend is kept after its last change:
// past any billing period's end.
const UsageKeep = 70 * 24 * time.Hour
