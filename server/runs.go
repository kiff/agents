package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
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
	Input  string
	// ID is the run's ID, when the caller names it; empty, a new one. A
	// caller that may start the same run more than once (a schedule's firing,
	// delivered at least once) names it, and the second Start returns the run
	// as stored with run.ErrExists. Never taken from a request.
	ID string
}

// runView is a run as the API shows it: no paused turn (it holds what tools
// returned, and exists only to continue the run) and no claim.
type runView struct {
	ID              string            `json:"id"`
	Agent           string            `json:"agent"`
	Session         string            `json:"session"`
	Trigger         run.Trigger       `json:"trigger"`
	Source          string            `json:"source,omitempty"`
	Status          run.Status        `json:"status"`
	Stop            string            `json:"stop,omitempty"`
	Output          string            `json:"output,omitempty"`
	Error           string            `json:"error,omitempty"`
	Cost            int64             `json:"cost_micro_usd"`
	Turns           int               `json:"turns"`
	Requirements    []run.Requirement `json:"requirements,omitempty"`
	CancelRequested bool              `json:"cancel_requested,omitempty"`
	Created         time.Time         `json:"created"`
	Updated         time.Time         `json:"updated"`
}

func viewRun(r run.Run) runView {
	return runView{ID: r.ID, Agent: r.Agent, Session: r.Session, Trigger: r.Trigger, Source: r.Source, Status: r.Status, Stop: r.Stop,
		Output: r.Output, Error: r.Error, Cost: r.Cost, Turns: r.Turns, Requirements: r.Requirements,
		CancelRequested: r.Cancel && !r.Status.Final(), Created: r.Created, Updated: r.Updated}
}

// agentRun is the tenant's run of the agent in the path; a run of another
// agent is not found.
func (s *Server) agentRun(ctx context.Context, c Caller, r *http.Request) (run.Run, error) {
	rn, err := s.Runs.Get(ctx, c.Tenant, r.PathValue("run_id"))
	if err != nil {
		return run.Run{}, err
	}
	if rn.Agent != r.PathValue("id") {
		return run.Run{}, run.ErrNotFound
	}
	return rn, nil
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
	for try := 1; ; try++ {
		rn, err := s.agentRun(ctx, c, r)
		if err != nil {
			return 0, nil, err
		}
		switch rn.Status {
		case run.Paused:
			next, err := s.cancelPaused(ctx, rn)
			if errors.Is(err, run.ErrConflict) && try < cancelTries {
				continue
			}
			if err != nil {
				return 0, nil, err
			}
			return http.StatusOK, viewRun(next), nil
		case run.Completed, run.Error, run.Cancelled:
			return 0, nil, conflict("the run has ended")
		}
		var expires time.Time
		if rn.Status == run.Pending {
			if expires, err = s.endedExpires(ctx, rn); err != nil {
				return 0, nil, err
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
		if err != nil {
			return 0, nil, err
		}
		return http.StatusOK, viewRun(next), nil
	}
}

// cancelPaused withdraws every call KIFF holds for a paused run, so an
// answer that comes later sends nothing, records what KIFF says became of
// each, and sets Cancel. The run is then resolved: it is handed to
// Background, which claims it, collects the outcomes (a withdrawn call comes
// back refused; one KIFF had already sent, its result) and ends it CANCELLED
// at its first checkpoint. If Background fails, the sweep continues it.
func (s *Server) cancelPaused(ctx context.Context, rn run.Run) (run.Run, error) {
	if s.Withdraw == nil {
		return run.Run{}, conflict("a paused run can't be cancelled here: nothing withdraws the calls KIFF holds")
	}
	for _, q := range rn.Requirements {
		if q.Exception == "" {
			return run.Run{}, conflict("a paused run can't be cancelled: KIFF holds a call it gave no ID to withdraw it by")
		}
	}
	got := map[string]string{}
	for _, q := range rn.Requirements {
		status, err := s.Withdraw(ctx, rn, q.Exception)
		if err != nil {
			return run.Run{}, fmt.Errorf("withdrawing held call %s: %w", q.CallID, err)
		}
		got[q.Exception] = status
	}
	next, err := run.Change(ctx, s.Runs, rn, s.now(), func(n *run.Run) error {
		n.Cancel = true
		for i := range n.Requirements {
			q := &n.Requirements[i]
			if st, ok := got[q.Exception]; ok && st != "" {
				q.Answer = st
				if q.Answered.IsZero() {
					q.Answered = s.now()
				}
			}
		}
		return nil
	})
	if err != nil {
		return run.Run{}, err
	}
	if s.Background != nil && resolved(next, s.now()) {
		if err := s.Background(context.WithoutCancel(ctx), next.Tenant, next.ID); err != nil && s.Log != nil {
			s.Log.ErrorContext(ctx, "continuing a cancelled paused run; the sweep will", "run", next.ID, "err", err)
		}
	}
	return next, nil
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
}

// The stream a run's start and resume share, as server-sent events:
//
//	event: run    the run as stored, once, first (start only)
//	event: event  one recorded session event of the run, its id the event's
//	              Seq: the cursor resume?after= takes
//	event: delta  what the agent does as it does it (an agent.Event): text as
//	              it is written, tool calls as they are made. No id: it is for
//	              showing, and the recorded events carry what is kept. Start
//	              only, in the foreground
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
	id := r.PathValue("id")
	if _, err := s.Agents.Get(ctx, c.Tenant, id); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.SessionID != "" {
		// A session is one agent's thread: another agent's is not found.
		sess, _, err := s.Sessions.Read(ctx, c.Tenant, body.SessionID)
		if err == nil && sess.Agent != id {
			err = session.ErrNotFound
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
	}
	rn, err := s.Runner.Start(ctx, Start{Tenant: c.Tenant, Agent: id, Session: body.SessionID, Trigger: run.ByOwner, Input: body.Input})
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

	sse := newStream(w)
	sse.send("run", viewRun(rn))
	f := &follower{s: s, sse: sse, tenant: c.Tenant, rn: rn}
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
	if err != nil {
		s.logError(r, "run failed", err)
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
			f.sse.event("event", strconv.FormatInt(e.Seq, 10), e)
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
	rn, err := s.Runner.Start(ctx, Start{Tenant: c.Tenant, Agent: sc.Agent, Trigger: run.BySchedule, Source: id, Input: sc.Input})
	if err != nil {
		return 0, nil, err
	}
	if err := s.toBackground(ctx, rn); err != nil {
		return 0, nil, err
	}
	return http.StatusAccepted, viewRun(rn), nil
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
