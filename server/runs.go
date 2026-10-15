package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
)

// MaxInput bounds what a run is asked, in bytes.
const MaxInput = 32_000

// followEvery is how often a resumed stream looks for more of a run.
const followEvery = 500 * time.Millisecond

// Runner starts and executes runs. The server owns HTTP, SSE and who the
// caller is; the Runner owns how a run is created, claimed, run and stored.
type Runner interface {
	// Start creates a PENDING run for the tenant's agent: in Session (an
	// existing conversation) or in a new session when it is empty. It
	// returns the run as stored; session.ErrNotFound when Session isn't the
	// tenant's.
	Start(ctx context.Context, s Start) (run.Run, error)
	// Execute claims the run and runs it to its next stop (answered, a cap,
	// paused on a hold, cancelled, failed), passing each event to emit, nil
	// in the background. It returns the run as stored at that stop.
	Execute(ctx context.Context, tenant, id string, emit func(agent.Event)) (run.Run, error)
}

// Start is what a run is started with.
type Start struct {
	Tenant, Agent, Session string
	Trigger                run.Trigger
	// Source is what started it within its trigger (run.Run.Source).
	Source string
	Input  string
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
// one stops at its next checkpoint. A PAUSED run waits on calls KIFF holds,
// and those must be withdrawn first: until that is built (#15), it is a
// conflict.
func (s *Server) cancelRun(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	rn, err := s.agentRun(ctx, c, r)
	if err != nil {
		return 0, nil, err
	}
	switch rn.Status {
	case run.Paused:
		return 0, nil, conflict("a paused run can't be cancelled yet: KIFF still holds its calls")
	case run.Completed, run.Error, run.Cancelled:
		return 0, nil, conflict("the run has ended")
	}
	next, err := run.Change(ctx, s.Runs, rn, s.now(), func(n *run.Run) error {
		if n.Status == run.Pending {
			n.Status = run.Cancelled
		}
		n.Cancel = true
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, viewRun(next), nil
}

// startBody is what POST /agents/{id}/runs takes.
type startBody struct {
	Input      string `json:"input"`
	SessionID  string `json:"session_id,omitempty"`
	Background bool   `json:"background,omitempty"`
}

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
	id := r.PathValue("id")
	if _, err := s.Agents.Get(ctx, c.Tenant, id); err != nil {
		s.fail(w, r, err)
		return
	}
	rn, err := s.Runner.Start(ctx, Start{Tenant: c.Tenant, Agent: id, Session: body.SessionID, Trigger: run.ByOwner, Input: body.Input})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Background {
		if err := s.Background(ctx, c.Tenant, rn.ID); err != nil {
			// Nothing will claim it: end it now rather than leave it to the
			// sweep, and say so.
			if _, cerr := run.Change(ctx, s.Runs, rn, s.now(), func(n *run.Run) error {
				n.Status, n.Error = run.Error, "it could not be started in the background"
				return nil
			}); cerr != nil {
				err = errors.Join(err, cerr)
			}
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusAccepted, viewRun(rn))
		return
	}
	sse := newStream(w)
	sse.send("run", viewRun(rn))
	end, err := s.Runner.Execute(ctx, c.Tenant, rn.ID, func(e agent.Event) { sse.send(string(e.Kind), e) })
	if err != nil {
		s.logError(r, "run failed", err)
		if got, gerr := s.Runs.Get(context.WithoutCancel(ctx), c.Tenant, rn.ID); gerr == nil {
			end = got
		} else {
			end.Status = "" // unknown: the client reads the run
		}
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
	sse := newStream(w)
	tick := time.NewTicker(followEvery)
	defer tick.Stop()
	for {
		_, events, err := s.Sessions.ReadRun(ctx, c.Tenant, rn.Session, rn.ID)
		if err != nil && !errors.Is(err, session.ErrNotFound) {
			s.logError(r, "resume", err)
			return
		}
		for _, e := range events {
			if e.Seq > after {
				sse.event("event", strconv.FormatInt(e.Seq, 10), e)
				after = e.Seq
			}
		}
		if rn.Status.Final() || rn.Status == run.Paused {
			sse.send("end", viewRun(rn))
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

// stream writes server-sent events.
type stream struct {
	w http.ResponseWriter
	f http.Flusher
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
	if id != "" {
		fmt.Fprintf(s.w, "id: %s\n", id)
	}
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, b)
	if s.f != nil {
		s.f.Flush()
	}
}

// scheduleRuns lists the runs one of the tenant's schedules started, newest
// first.
func (s *Server) scheduleRuns(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	id := r.PathValue("id")
	if _, err := s.Schedules.Store.Get(ctx, c.Tenant, id); err != nil {
		return 0, nil, err
	}
	limit := defaultSessions
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxSessions {
			return 0, nil, bad("limit is 1 to %d", maxSessions)
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
