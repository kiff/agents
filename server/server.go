// Package server is the HTTP API tafs cloud calls: agents, their memory,
// sessions and schedules, scoped by tenant.
//
// The tenant always comes from the verified caller (Auth), never from a
// request. Anything outside the caller's tenant is not found. Run routes join
// once runs are stored.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/schedule"
	"github.com/kiff/agents/session"
)

// maxBody bounds a request body. A definition is at most registry.MaxSize as
// stored, and JSON escaping can make its request a few times longer.
const maxBody = 4 << 20

// Session listing limits.
const (
	defaultSessions = 20
	maxSessions     = 100
)

// ErrUnauthorized means the request's caller couldn't be verified.
var ErrUnauthorized = errors.New("server: the caller is not verified")

// Caller is who made a request, as verified by Auth.
type Caller struct {
	Tenant string
}

// Server serves the API.
type Server struct {
	// Auth verifies a request's caller, or returns ErrUnauthorized.
	Auth func(r *http.Request) (Caller, error)

	Agents    registry.Agents
	Sessions  session.Store
	Notes     session.Notes
	Schedules *schedule.Service
	Runs      run.Runs
	// Runner starts and executes runs. Without one, runs can be read,
	// cancelled and followed, but not started.
	Runner Runner
	// Background has the run executed by another invocation, and returns
	// once that invocation is on its way.
	Background func(ctx context.Context, tenant, id string) error
	// Withdraw takes back a call KIFF holds for run r, by its exception ID,
	// and returns what became of the call: "invalidated" when it was
	// withdrawn, or KIFF's status for one it had already answered
	// (approved, refused, expired). It is safe to repeat. Nil: a paused run
	// can't be cancelled (409).
	Withdraw func(ctx context.Context, r run.Run, exception string) (string, error)
	// Gateways are the gateways a stored agent may name, as in
	// registry.Builder.
	Gateways []string

	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Log records what a caller isn't told: why a request failed inside. Nil
	// logs nothing.
	Log *slog.Logger
}

// Handler returns the API's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	route := func(pattern string, h func(context.Context, Caller, *http.Request) (int, any, error)) {
		mux.HandleFunc(pattern, s.serve(h))
	}
	route("GET /agents", s.listAgents)
	route("GET /agents/{id}", s.getAgent)
	route("PUT /agents/{id}", s.putAgent)
	route("DELETE /agents/{id}", s.deleteAgent)
	route("GET /agents/{id}/memories", s.listMemories)
	route("DELETE /agents/{id}/memories/{name}", s.forget)

	route("GET /sessions", s.listSessions)
	route("GET /sessions/{id}", s.getSession)
	route("DELETE /sessions/{id}", s.deleteSession)

	route("POST /schedules", s.createSchedule)
	route("GET /schedules", s.listSchedules)
	route("GET /schedules/{id}", s.getSchedule)
	route("PUT /schedules/{id}", s.updateSchedule)
	route("DELETE /schedules/{id}", s.deleteSchedule)
	route("POST /schedules/{id}/enable", func(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
		return s.switchSchedule(ctx, c, r, true)
	})
	route("POST /schedules/{id}/disable", func(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
		return s.switchSchedule(ctx, c, r, false)
	})
	route("GET /schedules/{id}/runs", s.scheduleRuns)

	if s.Runner != nil { // until runs can be executed, there is no starting one
		mux.HandleFunc("POST /agents/{id}/runs", s.startRun)
		mux.HandleFunc("POST /agents/{id}/runs/{run_id}/continue", s.continueRun)
		route("POST /schedules/{id}/trigger", s.triggerSchedule)
		route("POST /agents/{id}/events", s.startEvent)
	}
	route("GET /agents/{id}/runs/{run_id}", s.getRun)
	route("POST /agents/{id}/runs/{run_id}/cancel", s.cancelRun)
	mux.HandleFunc("POST /agents/{id}/runs/{run_id}/resume", s.resumeRun)
	return jsonErrors{mux}
}

// jsonErrors answers what the routes don't serve (a path the API doesn't
// have: 404; a method a path doesn't take: 405, with Allow) as every other
// error is answered, in JSON. The mux decides which: its own answer runs
// once, into a recorder, only for a request no route matches.
type jsonErrors struct{ mux *http.ServeMux }

func (j jsonErrors) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, pattern := j.mux.Handler(r); pattern != "" {
		j.mux.ServeHTTP(w, r)
		return
	}
	rec := &statusRecorder{header: http.Header{}}
	j.mux.ServeHTTP(rec, r)
	switch rec.code {
	case http.StatusNotFound:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case http.StatusMethodNotAllowed:
		w.Header().Set("Allow", rec.header.Get("Allow"))
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "this path doesn't take that method"})
	default: // a redirect to a path's clean form: the mux's own
		j.mux.ServeHTTP(w, r)
	}
}

// statusRecorder keeps a response's status and header, and drops its body.
type statusRecorder struct {
	header http.Header
	code   int
}

func (s *statusRecorder) Header() http.Header { return s.header }

func (s *statusRecorder) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.WriteHeader(http.StatusOK)
	return len(b), nil
}

// serve verifies the caller, runs h, and writes what it returns, or its
// error as the status it deserves.
func (s *Server) serve(h func(context.Context, Caller, *http.Request) (int, any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := s.Auth(r)
		if err == nil && c.Tenant == "" {
			err = ErrUnauthorized
		}
		if err != nil {
			s.fail(w, r, fmt.Errorf("%w: %w", ErrUnauthorized, err))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		status, body, err := h(r.Context(), c, r)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if body == nil {
			w.WriteHeader(status)
			return
		}
		writeJSON(w, status, body)
	}
}

// badRequest is an error the caller can fix: its message is theirs to read.
type badRequest struct{ err error }

func (b badRequest) Error() string { return b.err.Error() }
func (b badRequest) Unwrap() error { return b.err }

func bad(format string, args ...any) error { return badRequest{fmt.Errorf(format, args...)} }

// conflictError is a request the current state refuses: its message says
// what to do instead.
type conflictError struct{ msg string }

func (c conflictError) Error() string { return c.msg }

func conflict(msg string) error { return conflictError{msg} }

// unavailableError is a request this server isn't set up to serve.
type unavailableError struct{ msg string }

func (u unavailableError) Error() string { return u.msg }

func unavailable(msg string) error { return unavailableError{msg} }

// fail writes err as a status and a message. Only errors the caller can act
// on say what went wrong; anything else is logged, and the caller reads
// "internal error".
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var br badRequest
	var cf conflictError
	var un unavailableError
	var tooBig *http.MaxBytesError
	status, msg := http.StatusInternalServerError, "internal error"
	switch {
	case errors.Is(err, ErrUnauthorized):
		status, msg = http.StatusUnauthorized, "the caller is not verified"
	case errors.Is(err, registry.ErrNotFound), errors.Is(err, schedule.ErrNotFound), errors.Is(err, session.ErrNotFound),
		errors.Is(err, run.ErrNotFound):
		status, msg = http.StatusNotFound, "not found"
	case errors.Is(err, run.ErrConflict), errors.Is(err, run.ErrTransition):
		status, msg = http.StatusConflict, "the run changed meanwhile, or can't take that action now; read it again"
	case errors.As(err, &tooBig):
		status, msg = http.StatusRequestEntityTooLarge, "the body is too large"
	case errors.As(err, &un):
		status, msg = http.StatusServiceUnavailable, un.Error()
	case errors.As(err, &cf):
		status, msg = http.StatusConflict, cf.Error()
	case errors.As(err, &br):
		status, msg = http.StatusBadRequest, br.Error()
	case errors.Is(err, schedule.ErrNoAgent):
		status, msg = http.StatusBadRequest, "the tenant has no such agent"
	}
	if status >= 500 || status == http.StatusUnauthorized {
		if s.Log != nil {
			s.Log.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "status", status, "err", err)
		}
	}
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// decode reads a JSON body into v, refusing unknown fields and anything after
// the value.
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return err
		}
		return bad("the body is not the JSON this route takes: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return bad("the body holds more than one JSON value")
	}
	return nil
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Agents.

func (s *Server) listAgents(ctx context.Context, c Caller, _ *http.Request) (int, any, error) {
	list, err := s.Agents.List(ctx, c.Tenant)
	if err != nil {
		return 0, nil, err
	}
	if list == nil {
		list = []registry.Definition{}
	}
	return http.StatusOK, map[string]any{"agents": list}, nil
}

func (s *Server) getAgent(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	d, err := s.Agents.Get(ctx, c.Tenant, r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, d, nil
}

// putAgent stores a definition. Its tenant is the caller's and its ID the
// path's; a body that names others is refused rather than read.
func (s *Server) putAgent(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	var d registry.Definition
	if err := decode(r, &d); err != nil {
		return 0, nil, err
	}
	id := r.PathValue("id")
	if d.Tenant != "" && d.Tenant != c.Tenant {
		return 0, nil, bad("the tenant comes from the caller, not the body")
	}
	if d.ID != "" && d.ID != id {
		return 0, nil, bad("the id in the body is not the path's")
	}
	d.Tenant, d.ID = c.Tenant, id
	if err := d.Check(); err != nil {
		return 0, nil, badRequest{err}
	}
	if !slices.Contains(s.Gateways, d.Gateway) {
		return 0, nil, badRequest{registry.ErrGateway}
	}
	if err := s.Agents.Put(ctx, d); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, d, nil
}

// deleteAgent removes an agent and everything kept for it: its schedules,
// then its runs, then its sessions and notes, then its definition, last, so a
// retry after a failure still finds the agent and carries on.
func (s *Server) deleteAgent(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	id := r.PathValue("id")
	if _, err := s.Agents.Get(ctx, c.Tenant, id); err != nil {
		return 0, nil, err
	}
	if err := s.Schedules.DeleteAgent(ctx, c.Tenant, id); err != nil {
		return 0, nil, err
	}
	if err := s.withdrawAll(ctx, c.Tenant, id); err != nil {
		return 0, nil, err
	}
	if err := s.Runs.DeleteAgent(ctx, c.Tenant, id); err != nil {
		return 0, nil, err
	}
	if err := session.Remove(ctx, s.Sessions, s.Notes, c.Tenant, id); err != nil {
		return 0, nil, err
	}
	if err := s.Agents.Delete(ctx, c.Tenant, id); err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}

// withdrawAll withdraws every call KIFF holds for the agent's paused runs,
// before they are deleted: an answer that comes later sends nothing for a
// run that is gone. It needs the agent, for its KIFF key, so it runs before
// the agent is deleted. Without Withdraw, nothing is withdrawn.
func (s *Server) withdrawAll(ctx context.Context, tenant, agent string) error {
	if s.Withdraw == nil {
		return nil
	}
	runs, err := s.Runs.List(ctx, tenant, agent, 0)
	if err != nil {
		return err
	}
	for _, r := range runs {
		if r.Status != run.Paused {
			continue
		}
		for _, q := range r.Requirements {
			if q.Exception == "" {
				continue // KIFF gave it no ID to withdraw it by
			}
			if _, err := s.Withdraw(ctx, r, q.Exception); err != nil {
				return fmt.Errorf("withdrawing held call %s of run %s: %w", q.CallID, r.ID, err)
			}
		}
	}
	return nil
}

// Memory.

func (s *Server) listMemories(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	id := r.PathValue("id")
	if _, err := s.Agents.Get(ctx, c.Tenant, id); err != nil {
		return 0, nil, err
	}
	notes, err := s.Notes.ListNotes(ctx, c.Tenant, id)
	if err != nil {
		return 0, nil, err
	}
	if notes == nil {
		notes = []session.Note{}
	}
	return http.StatusOK, map[string]any{"memories": notes}, nil
}

func (s *Server) forget(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	id := r.PathValue("id")
	if _, err := s.Agents.Get(ctx, c.Tenant, id); err != nil {
		return 0, nil, err
	}
	if err := s.Notes.DeleteNote(ctx, c.Tenant, id, r.PathValue("name")); err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}

// Sessions.

func (s *Server) listSessions(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	agent := r.URL.Query().Get("agent_id")
	if agent == "" {
		return 0, nil, bad("agent_id is required")
	}
	limit := defaultSessions
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxSessions {
			return 0, nil, bad("limit is 1 to %d", maxSessions)
		}
		limit = n
	}
	list, err := s.Sessions.List(ctx, c.Tenant, agent, limit)
	if err != nil {
		return 0, nil, err
	}
	if list == nil {
		list = []session.Session{}
	}
	return http.StatusOK, map[string]any{"sessions": list}, nil
}

func (s *Server) getSession(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	sess, events, err := s.Sessions.Read(ctx, c.Tenant, r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	if events == nil {
		events = []session.Event{}
	}
	return http.StatusOK, map[string]any{"session": sess, "events": events}, nil
}

// deleteSession leaves "not found" to the Store's Delete, which also
// finishes a session whose deletion was cut short; a Read first would hide
// that one, and it could never be finished.
func (s *Server) deleteSession(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	if err := s.Sessions.Delete(ctx, c.Tenant, r.PathValue("id")); err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}

// Schedules.

// scheduleView is a schedule as the API shows it: with its next firing, which
// the app shows and the Scheduler doesn't report.
type scheduleView struct {
	schedule.Schedule
	Next *time.Time `json:"next,omitempty"`
}

func (s *Server) view(sc schedule.Schedule) scheduleView {
	v := scheduleView{Schedule: sc}
	if next := sc.Next(s.now()); !next.IsZero() {
		next = next.UTC() // like every time the API returns
		v.Next = &next
	}
	return v
}

// readSchedule reads a schedule from the body. Its tenant is the caller's; a
// body that names another is refused rather than read.
func readSchedule(r *http.Request, c Caller) (schedule.Schedule, error) {
	var sc schedule.Schedule
	if err := decode(r, &sc); err != nil {
		return sc, err
	}
	if sc.Tenant != "" && sc.Tenant != c.Tenant {
		return sc, bad("the tenant comes from the caller, not the body")
	}
	sc.Tenant = c.Tenant
	return sc, nil
}

// putSchedule stores and sets sc; a schedule that fails its check is the
// caller's to fix.
func (s *Server) putSchedule(ctx context.Context, sc schedule.Schedule) error {
	if err := sc.Check(); err != nil {
		return badRequest{err}
	}
	return s.Schedules.Put(ctx, sc)
}

// createSchedule makes a new schedule; one with that ID already is a
// conflict, so a create never replaces.
func (s *Server) createSchedule(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	sc, err := readSchedule(r, c)
	if err != nil {
		return 0, nil, err
	}
	switch _, err := s.Schedules.Store.Get(ctx, c.Tenant, sc.ID); {
	case err == nil:
		return 0, nil, conflict("a schedule with that id exists; update it instead")
	case !errors.Is(err, schedule.ErrNotFound):
		return 0, nil, err
	}
	if err := s.putSchedule(ctx, sc); err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, s.view(sc), nil
}

func (s *Server) listSchedules(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	all, err := s.Schedules.Store.List(ctx, c.Tenant)
	if err != nil {
		return 0, nil, err
	}
	agent := r.URL.Query().Get("agent_id")
	out := []scheduleView{}
	for _, sc := range all {
		if agent == "" || sc.Agent == agent {
			out = append(out, s.view(sc))
		}
	}
	return http.StatusOK, map[string]any{"schedules": out}, nil
}

func (s *Server) getSchedule(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	sc, err := s.Schedules.Store.Get(ctx, c.Tenant, r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, s.view(sc), nil
}

// updateSchedule replaces a schedule that exists.
func (s *Server) updateSchedule(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	id := r.PathValue("id")
	if _, err := s.Schedules.Store.Get(ctx, c.Tenant, id); err != nil {
		return 0, nil, err
	}
	sc, err := readSchedule(r, c)
	if err != nil {
		return 0, nil, err
	}
	if sc.ID != "" && sc.ID != id {
		return 0, nil, bad("the id in the body is not the path's")
	}
	sc.ID = id
	if err := s.putSchedule(ctx, sc); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, s.view(sc), nil
}

func (s *Server) deleteSchedule(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	id := r.PathValue("id")
	if _, err := s.Schedules.Store.Get(ctx, c.Tenant, id); err != nil {
		return 0, nil, err
	}
	if err := s.Schedules.Delete(ctx, c.Tenant, id); err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}

func (s *Server) switchSchedule(ctx context.Context, c Caller, r *http.Request, on bool) (int, any, error) {
	sc, err := s.Schedules.Store.Get(ctx, c.Tenant, r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	sc.Enabled = on
	if err := s.putSchedule(ctx, sc); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, s.view(sc), nil
}
