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
	"strings"
	"time"

	"github.com/kiff/agents/attach"
	"github.com/kiff/agents/drafts"
	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/schedule"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/skill"
	"github.com/kiff/agents/usage"
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
	// Drafts claims a confirmed taf draft's key (#91); nil: no such route.
	Drafts Drafts
	// Skills keeps the tenants' skill packages (#101). Nil: no skills can be
	// stored, and an agent naming one is refused.
	Skills skill.Store
	// Background has the run executed by another invocation, and returns
	// once that invocation is on its way.
	Background func(ctx context.Context, tenant, id string) error
	// Operator verifies that a request is an operator's, for the read-only
	// operators' routes under /ops. Nil: those routes aren't served.
	Operator func(r *http.Request) error
	// Withdraw takes back a call KIFF holds for run r, by its exception ID,
	// and returns what became of the call: "invalidated" when it was
	// withdrawn, or KIFF's status for one it had already answered
	// (approved, refused, expired). It is safe to repeat. Nil: a paused run
	// can't be cancelled (409).
	Withdraw func(ctx context.Context, r run.Run, exception string) (string, error)
	// Files are the files people attach to their chat messages
	// (kiff/tafs#209). Nil: the attachment routes answer 503, and removing a
	// session, an agent or a tenant has no files to remove.
	Files *attach.Service
	// Access answers whether a member may use or oversee an agent (#221): a
	// member reads their sessions with it, and writes into their runs, only
	// if they may. Nil: nothing is checked. The runner checks runs.
	Access run.AccessFunc
	// ResolveGrant asks the account who a run grant was signed for, for
	// this agent: the person's KIFF subject. A grant KIFF doesn't vouch
	// for is an error. Nil: grants aren't taken.
	ResolveGrant func(ctx context.Context, tenant, agent, grant string) (string, error)
	// ResolveReadGrant asks the account what a read grant for this agent
	// allows: the one way to read another person's sessions, which the
	// account records before it grants. Nil: no read grant is taken, and
	// a request with one is refused.
	ResolveReadGrant run.ReadGrantFunc
	// RequireGrant refuses a member's run without a grant: once the
	// account mints them, a member is who KIFF says, not who the caller
	// asserts.
	RequireGrant bool
	// Answers reads from KIFF, with the run's agent's own key, what it
	// answered on run r's held calls, by exception (approved, refused,
	// expired, invalidated), leaving out any still waiting: a continue asks
	// it when no alert has said yet (kiff/tafs#206). Nil: a continue waits
	// for the alert or the hold's expiry.
	Answers func(ctx context.Context, r run.Run) (map[string]string, error)
	// Usage keeps what each account's chats cost, for AI fair use
	// (kiff/tafs#193). Nil: a chat that asks to be counted is refused.
	Usage usage.Store
	// Erase deletes what the tenant keeps outside the stores when its account
	// is deleted (kiff/tafs#185): its sealing key and its drafts' claim
	// records. It is safe to repeat. Nil: nothing outside the stores is
	// erased.
	Erase func(ctx context.Context, tenant string) error
	// EndKIFFTrial ends t's trial Card in KIFF with the agent's own key, and
	// returns KIFF's counts of its calls (kiff/tafs#173). Nil: idle trials
	// aren't swept, since ending one here would leave its Cards in KIFF.
	EndKIFFTrial func(ctx context.Context, t registry.Trial) (registry.TrialCounts, error)
	// MaxActive caps a tenant's active runs (run.Run.Active: PENDING or
	// RUNNING): a request that would start one more is 429. Zero: no cap.
	// Only requests are capped; a schedule's firing and the sweep's continue
	// have nobody to answer, and go on. Two requests at once may both pass
	// the count, so the cap can be overrun by the requests in flight.
	MaxActive int
	// Gateways are the gateways a stored agent may name, as in
	// registry.Builder.
	Gateways []string
	// NotifyURLs are the URLs a run started through the API may be
	// announced to when it ends or pauses (POST /agents/{id}/runs, notify),
	// exactly; none, no run is.
	NotifyURLs []string
	// LiveChat is how long after its last message a requester's chat is
	// still the one a message goes on in, when it asks for its live chat
	// (POST /agents/{id}/runs, chat "live"). Zero is DefaultLiveChat.
	LiveChat time.Duration
	// FloodMessages is how many messages one requester may send one agent
	// in FloodWindow (#219): one busy person doesn't hold back the rest.
	// Zero is DefaultFloodMessages; FloodWindow zero is DefaultFloodWindow.
	FloodMessages int
	FloodWindow   time.Duration

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
	route("DELETE /tenant", s.eraseTenant)
	route("GET /agents/{id}/versions", s.listVersions)
	route("POST /agents/{id}/versions", s.putVersion)
	route("GET /agents/{id}/versions/{n}", s.getVersion)
	route("POST /agents/{id}/versions/{n}/live", s.switchVersion)
	route("POST /agents/{id}/versions/{n}/pending", s.pendingVersion)
	route("DELETE /agents/{id}/switching", s.cancelSwitch)
	route("GET /agents/{id}/halt", s.getHalt)
	route("POST /agents/{id}/halt", s.halt)
	route("POST /agents/{id}/resume", s.resume)
	route("GET /halt", s.getHalt)
	route("POST /halt", s.halt)
	route("POST /resume", s.resume)
	route("GET /agents/{id}/trials/{run_id}", s.getTrial)
	route("GET /usage/ai", s.getAIUsage)
	route("PUT /agents/{id}/trial", s.openTrial)
	route("GET /agents/{id}/trial", s.getOpenTrial)
	route("DELETE /agents/{id}/trial", s.closeTrial)
	route("GET /skills", s.listSkills)
	route("GET /skills/{name}/{version}", s.getSkill)
	route("PUT /skills/{name}/{version}", s.putSkill)
	route("GET /agents/{id}/memories", s.listMemories)
	route("DELETE /agents/{id}/memories/{name}", s.forget)
	route("POST /agents/{id}/memories/{name}/share", s.shareMemory)

	route("GET /sessions", s.listSessions)
	route("GET /sessions/{id}", s.getSession)
	route("DELETE /sessions/{id}", s.deleteSession)
	route("POST /agents/{id}/attachments", s.createAttachment)
	route("GET /agents/{id}/attachments/{att}", s.readAttachment)
	route("POST /sessions/{id}/archive", s.archiveSession(true))
	route("POST /sessions/{id}/unarchive", s.archiveSession(false))

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
		// The same, for the run that paused on a held call (kiff/tafs#206): what
		// the app that answered the hold knows.
		mux.HandleFunc("POST /agents/{id}/holds/{exception}/continue", s.continueRun)
		// A card the run waits on, answered as data, never as chat text.
		mux.HandleFunc("POST /agents/{id}/runs/{run_id}/cards/{card}/answer", s.answerCard)
		route("POST /schedules/{id}/trigger", s.triggerSchedule)
		route("POST /agents/{id}/events", s.startEvent)
		mux.HandleFunc("POST /agents/{id}/versions/{n}/trial", s.startTrial)
		mux.HandleFunc("POST /agents/{id}/trial/messages", s.trialMessage)
	}
	if s.Drafts != nil {
		route("POST /drafts/{draft_id}/claim", s.claimDraft)
	}
	route("GET /agents/{id}/runs", s.listRuns)
	route("GET /agents/{id}/runs/{run_id}", s.getRun)
	route("POST /agents/{id}/runs/{run_id}/cancel", s.cancelRun)
	route("POST /agents/{id}/runs/{run_id}/messages", s.steerRun)
	mux.HandleFunc("POST /agents/{id}/runs/{run_id}/resume", s.resumeRun)
	s.opsRoutes(mux)
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

// tooManyError is a request over the tenant's cap on active runs.
type tooManyError struct{ n, max int }

func (t tooManyError) Error() string {
	return fmt.Sprintf("the tenant has %d runs going and may have %d: wait for one to end, then try again", t.n, t.max)
}

// floodError is a requester's message past their own flood guard.
type floodError struct {
	n      int
	window time.Duration
}

func (f floodError) Error() string {
	return fmt.Sprintf("that's %d messages in %d minutes; wait a little before writing again", f.n, int(f.window.Minutes()))
}

// RetryAfter is how long a 429 asks the caller to wait, in seconds.
const RetryAfter = 30

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
	var tm tooManyError
	var fl floodError
	var tooBig *http.MaxBytesError
	status, msg := http.StatusInternalServerError, "internal error"
	if errors.Is(err, run.ErrNoOversight) {
		// A read of another person's work the account didn't allow.
		if s.Log != nil {
			s.Log.InfoContext(r.Context(), "no read grant", "method", r.Method, "path", r.URL.Path, "err", err)
		}
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "no_access", "need": "oversee"})
		return
	}
	if errors.Is(err, run.ErrNoAccess) {
		// The account says this person may not use the agent (#221), or
		// couldn't say: the app says so, and nothing more.
		if s.Log != nil {
			s.Log.InfoContext(r.Context(), "no access", "method", r.Method, "path", r.URL.Path, "err", err)
		}
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "no_access", "need": "use"})
		return
	}
	switch {
	case errors.Is(err, ErrUnauthorized):
		status, msg = http.StatusUnauthorized, "the caller is not verified"
	case errors.Is(err, registry.ErrNotFound), errors.Is(err, registry.ErrNoVersion), errors.Is(err, schedule.ErrNotFound), errors.Is(err, session.ErrNotFound),
		errors.Is(err, run.ErrNotFound), errors.Is(err, drafts.ErrNotFound), errors.Is(err, skill.ErrNotFound):
		status, msg = http.StatusNotFound, "not found"
	case errors.Is(err, registry.ErrNoTrial):
		status, msg = http.StatusNotFound, "this agent has no open trial"
	case errors.Is(err, registry.ErrSwitching):
		status, msg = http.StatusConflict, "this agent is switching versions: nothing of it runs, and its definition stays, until the switch is done or cancelled"
	case errors.Is(err, ErrAIFairUse):
		status, msg = http.StatusConflict, ErrAIFairUse.Error()
	case errors.Is(err, registry.ErrHalted):
		status, msg = http.StatusConflict, "halted: this agent, or its whole account, is halted; nothing of it runs until it is resumed"
	case errors.Is(err, registry.ErrSwitchChanged):
		status, msg = http.StatusConflict, "the switch changed meanwhile; read it again"
	case errors.Is(err, registry.ErrNoSwitch):
		status, msg = http.StatusConflict, "this agent isn't switching to that version: begin the switch first"
	case errors.Is(err, run.ErrConflict), errors.Is(err, run.ErrTransition):
		status, msg = http.StatusConflict, "the run changed meanwhile, or can't take that action now; read it again"
	case errors.As(err, &tooBig):
		status, msg = http.StatusRequestEntityTooLarge, "the body is too large"
	case errors.As(err, &tm):
		status, msg = http.StatusTooManyRequests, tm.Error()
		w.Header().Set("Retry-After", strconv.Itoa(RetryAfter))
	case errors.As(err, &fl):
		status, msg = http.StatusTooManyRequests, fl.Error()
		w.Header().Set("Retry-After", strconv.Itoa(RetryAfter))
	case errors.As(err, &un):
		status, msg = http.StatusServiceUnavailable, un.Error()
	case errors.As(err, &cf):
		status, msg = http.StatusConflict, cf.Error()
	case errors.As(err, &br):
		status, msg = http.StatusBadRequest, br.Error()
	case errors.Is(err, schedule.ErrNoAgent):
		status, msg = http.StatusBadRequest, "the tenant has no such agent"
	case errors.Is(err, attach.ErrNotFound):
		status, msg = http.StatusNotFound, "not found"
	case errors.Is(err, attach.ErrBound):
		status, msg = http.StatusConflict, "a file was already sent with another message: attach it again"
	case errors.Is(err, attach.ErrInvalid):
		status, msg = http.StatusBadRequest, strings.TrimPrefix(err.Error(), attach.ErrInvalid.Error()+": ")
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
	if err := s.checkSkills(ctx, c.Tenant, d.Skills); err != nil {
		return 0, nil, err
	}
	// A switch under way waits for its own version (kiff/tafs#107): nothing
	// else replaces the definition meanwhile, an upgrade included.
	if _, on, err := s.Agents.Switching(ctx, c.Tenant, d.ID); err != nil {
		return 0, nil, err
	} else if on {
		return 0, nil, registry.ErrSwitching
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
	if err := s.removeAgent(ctx, c.Tenant, id); err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}

// removeAgent deletes the agent and everything kept for it: its schedules,
// its runs (after withdrawing the calls KIFF holds for them), its sessions
// and notes, and its definition with its versions. The definition goes last,
// so a removal cut short is finished by repeating it.
func (s *Server) removeAgent(ctx context.Context, tenant, id string) error {
	if err := s.Schedules.DeleteAgent(ctx, tenant, id); err != nil {
		return err
	}
	if err := s.withdrawAll(ctx, tenant, id); err != nil {
		return err
	}
	if err := s.Runs.DeleteAgent(ctx, tenant, id); err != nil {
		return err
	}
	if s.Files != nil {
		if err := s.Files.RemoveAgent(ctx, tenant, id); err != nil {
			return err
		}
	}
	if err := session.Remove(ctx, s.Sessions, s.Notes, tenant, id); err != nil {
		return err
	}
	return s.Agents.Delete(ctx, tenant, id)
}

// tenantEraser is a store that deletes what it keeps for a whole tenant
// beyond its agents, such as the tenant's own halt or its skills.
type tenantEraser interface {
	EraseTenant(ctx context.Context, tenant string) error
}

// eraseTenant is tafs cloud deleting everything kept for the caller's
// account, when the owner deletes it (kiff/tafs#185, App Store Review
// Guideline 5.1.1(v)): every agent with what is kept for it, as removing
// each would, then what the tenant keeps beyond its agents, in the stores
// and outside them (Erase). It is safe to repeat, so a deletion cut short is
// finished by asking again.
func (s *Server) eraseTenant(ctx context.Context, c Caller, _ *http.Request) (int, any, error) {
	defs, err := s.Agents.List(ctx, c.Tenant)
	if err != nil {
		return 0, nil, err
	}
	for _, d := range defs {
		if err := s.removeAgent(ctx, c.Tenant, d.ID); err != nil && !errors.Is(err, registry.ErrNotFound) {
			return 0, nil, fmt.Errorf("erasing agent %s: %w", d.ID, err)
		}
	}
	for _, st := range []any{s.Agents, s.Skills, s.Usage} {
		if e, ok := st.(tenantEraser); ok {
			if err := e.EraseTenant(ctx, c.Tenant); err != nil {
				return 0, nil, err
			}
		}
	}
	if s.Files != nil {
		if err := s.Files.EraseTenant(ctx, c.Tenant); err != nil {
			return 0, nil, err
		}
	}
	if s.Erase != nil {
		if err := s.Erase(ctx, c.Tenant); err != nil {
			return 0, nil, err
		}
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
	// owner: a person's view (#219), the shared notes and their own;
	// without it, every note, each with its scope.
	owner, err := ownerParam(r)
	if err != nil {
		return 0, nil, err
	}
	notes, err := s.Notes.ListNotes(ctx, c.Tenant, id)
	if err != nil {
		return 0, nil, err
	}
	if owner != "" {
		notes = session.NotesFor(notes, owner)
	}
	out := make([]noteView, len(notes))
	for i, n := range notes {
		out[i] = noteView{Note: n, Quarantined: n.Quarantined()}
	}
	return http.StatusOK, map[string]any{"memories": out}, nil
}

// noteView is a note as listed: quarantined when its origin isn't known, so
// someone who manages the agent can share it on purpose or forget it.
type noteView struct {
	session.Note
	Quarantined bool `json:"quarantined,omitempty"`
}

func (s *Server) forget(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	id := r.PathValue("id")
	if _, err := s.Agents.Get(ctx, c.Tenant, id); err != nil {
		return 0, nil, err
	}
	// scope: a requester's note (#219), "member:<subject>"; without it, the
	// shared one of that name.
	scope, err := scopeParam(r)
	if err != nil {
		return 0, nil, err
	}
	name, err := noteName(r)
	if err != nil {
		return 0, nil, err
	}
	// Forgotten as the person, only their own note, or as the service.
	owner, err := ownerParam(r)
	if err != nil {
		return 0, nil, err
	}
	if owner != "" && scope != owner {
		return 0, nil, session.ErrNotFound
	}
	if err := s.Notes.DeleteNote(ctx, c.Tenant, id, scope, name); err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}

// shareMemory makes one requester's note shared (#219): every run of the
// agent reads it from then on. It keeps where the note came from, and the
// requester no longer has it as their own. A shared note of that name with
// other text is a conflict: forget it first.
func (s *Server) shareMemory(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	id := r.PathValue("id")
	if _, err := s.Agents.Get(ctx, c.Tenant, id); err != nil {
		return 0, nil, err
	}
	name, err := noteName(r)
	if err != nil {
		return 0, nil, err
	}
	scope, err := scopeParam(r)
	if err != nil {
		return 0, nil, err
	}
	// Shared as the note's own person, or as the service; a quarantined
	// note is released only by the service, naming who decided (by).
	owner, err := ownerParam(r)
	if err != nil {
		return 0, nil, err
	}
	if owner != "" && owner != scope {
		return 0, nil, session.ErrNotFound
	}
	notes, err := s.Notes.ListNotes(ctx, c.Tenant, id)
	if err != nil {
		return 0, nil, err
	}
	if scope == session.Shared {
		return s.releaseNote(ctx, r, notes, name)
	}
	var own, shared *session.Note
	for i, n := range notes {
		switch {
		case n.Name != name:
		case n.Scope == scope:
			own = &notes[i]
		case n.Scope == session.Shared:
			shared = &notes[i]
		}
	}
	if own == nil {
		return 0, nil, session.ErrNotFound
	}
	if shared != nil && shared.Text != own.Text {
		return 0, nil, conflict("a shared note of that name already says something else: forget it first")
	}
	n := *own
	n.Scope = session.Shared
	if n.Requester == "" {
		n.Requester = scope
	}
	n.SharedBy = scope
	// Shared first: a failure between the two leaves the note in both
	// places, never in neither, and sharing again finishes it.
	if err := s.Notes.PutNote(ctx, n); err != nil {
		return 0, nil, err
	}
	if err := s.Notes.DeleteNote(ctx, c.Tenant, id, scope, name); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, n, nil
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
	// owner: one requester's own sessions only (#219);
	// without it, every session of the agent, as before.
	owner, err := ownerParam(r)
	if err != nil {
		return 0, nil, err
	}
	if grant := readGrantHeader(r); grant != "" {
		// Another person's sessions, listed: their titles and times, never
		// what's in them. Each one read needs its own grant.
		err = s.mayReadAnothers(ctx, r, c.Tenant, agent, owner, "", grant)
	} else {
		err = s.mayRead(ctx, c.Tenant, agent, owner)
	}
	if err != nil {
		return 0, nil, err
	}
	var list []session.Session
	if owner != "" {
		list, err = s.Sessions.ListOwned(ctx, c.Tenant, agent, owner, limit)
	} else {
		list, err = s.Sessions.List(ctx, c.Tenant, agent, limit)
	}
	if err != nil {
		return 0, nil, err
	}
	if list == nil {
		list = []session.Session{}
	}
	return http.StatusOK, map[string]any{"sessions": list}, nil
}

// noteName is the note named in the path. No note's name starts with "@"
// (the memory tool takes none): a store may key a requester's notes so.
func noteName(r *http.Request) (string, error) {
	name := r.PathValue("name")
	if strings.HasPrefix(name, "@") {
		return "", bad("no note's name starts with @")
	}
	return name, nil
}

// releaseNote shares a quarantined note on purpose: the person named by
// "by" (member:<subject>), who manages the agent, says every run may read it.
// Only a quarantined note is released; a note shared already is a conflict.
func (s *Server) releaseNote(ctx context.Context, r *http.Request, notes []session.Note, name string) (int, any, error) {
	by := r.URL.Query().Get("by")
	kind, sub, _ := strings.Cut(by, ":")
	if q := (run.Requester{Kind: run.RequesterKind(kind), ID: sub}); q.Kind != run.ByMember || !q.Valid() {
		return 0, nil, bad("scope names whose note to share, as in member:<subject>, or by names who shares a quarantined one")
	}
	for _, n := range notes {
		if n.Scope != session.Shared || n.Name != name {
			continue
		}
		if !n.Quarantined() {
			return 0, nil, conflict("that note is shared already")
		}
		n.SharedBy = by
		if err := s.Notes.PutNote(ctx, n); err != nil {
			return 0, nil, err
		}
		return http.StatusOK, n, nil
	}
	return 0, nil, session.ErrNotFound
}

// scopeParam is the "scope" query parameter, a requester's key, checked as
// owner is; empty is the shared scope.
func scopeParam(r *http.Request) (string, error) {
	v := r.URL.Query().Get("scope")
	if v == "" {
		return session.Shared, nil
	}
	kind, id, _ := strings.Cut(v, ":")
	if q := (run.Requester{Kind: run.RequesterKind(kind), ID: id}); !q.Valid() {
		return "", bad("scope is a requester's kind and id, as in member:<subject>")
	}
	return v, nil
}

// mayRead is run.ErrNoAccess unless owner, when a member, may use the agent
// (#221): their own sessions, which overseeing it never adds to. Any other
// owner, or none, reads as before.
func (s *Server) mayRead(ctx context.Context, tenant, agent, owner string) error {
	kind, id, _ := strings.Cut(owner, ":")
	if s.Access == nil || run.RequesterKind(kind) != run.ByMember {
		return nil
	}
	a, err := s.Access(ctx, tenant, agent, id)
	if err != nil {
		return fmt.Errorf("%w: the account couldn't say: %v", run.ErrNoAccess, err)
	}
	if !a.Use {
		return run.ErrNoAccess
	}
	return nil
}

// AsService is the as parameter's one value: the caller reads or writes as
// the service itself, not as a person, so nothing is filtered to anyone's
// own. Only the service's own callers send it, for what no person asked.
const AsService = "service"

// asService is a bad request unless r says, by name, that it acts as the
// service: a person's content is read with their owner, or as the service,
// never by leaving the person out.
func asService(r *http.Request) error {
	if r.URL.Query().Get("as") != AsService {
		return bad("name the person (owner=member:<subject>) or read as the service (as=service)")
	}
	return nil
}

// notBoth is a bad request when a call that names a person also says it
// acts as the service: one or the other, never both, on writes as on reads.
func notBoth(r *http.Request) error {
	if r.URL.Query().Has("as") {
		return bad("name the person (requester) or as=service, not both")
	}
	return nil
}

// ReadGrantHeader carries a read grant: the account's word that the
// caller's person may read another person's sessions with the agent. A
// header, so the grant stays out of logged URLs.
const ReadGrantHeader = "Kiff-Read-Grant"

func readGrantHeader(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(ReadGrantHeader))
}

// mayReadAnothers is run.ErrNoOversight unless the account resolves grant
// to a read of owner's work with the agent, by the member the "reader"
// parameter names: their list when session is empty, else that one
// session, which owner must hold. The account checks the reader may read
// that work and recorded the read before granting it. It reads only:
// nothing with a read grant writes into a session. A read served is
// logged with its purpose, so it can be matched to the account's record.
func (s *Server) mayReadAnothers(ctx context.Context, r *http.Request, tenant, agent, owner, session, grant string) error {
	kind, id, _ := strings.Cut(owner, ":")
	if run.RequesterKind(kind) != run.ByMember {
		return bad("a read grant reads a member's sessions: owner is member:<subject>")
	}
	rkind, reader, _ := strings.Cut(r.URL.Query().Get("reader"), ":")
	if q := (run.Requester{Kind: run.RequesterKind(rkind), ID: reader}); q.Kind != run.ByMember || !q.Valid() {
		return bad("a read grant needs its reader: reader is member:<subject>")
	}
	if s.ResolveReadGrant == nil {
		return fmt.Errorf("%w: read grants aren't taken here", run.ErrNoOversight)
	}
	g, err := s.ResolveReadGrant(ctx, tenant, agent, grant)
	if err != nil {
		return fmt.Errorf("%w: %v", run.ErrNoOversight, err)
	}
	if g.Owner != id || g.Session != session {
		return fmt.Errorf("%w: the grant is for another read", run.ErrNoOversight)
	}
	if g.Reader != reader {
		return fmt.Errorf("%w: the grant is another person's", run.ErrNoOversight)
	}
	if s.Log != nil {
		s.Log.InfoContext(ctx, "read served", "read", g.Read, "agent", agent, "reader", g.Reader, "owner", g.Owner, "session", g.Session, "purpose", g.Purpose)
	}
	return nil
}

// ownerParam is the "owner" query parameter, a requester's key
// ("member:<subject>"), checked. Without one, the caller must say it reads
// as the service itself (as=service): a missing owner never means everyone.
func ownerParam(r *http.Request) (string, error) {
	v := r.URL.Query().Get("owner")
	if v == "" {
		return "", asService(r)
	}
	if r.URL.Query().Get("as") != "" {
		return "", bad("name the person (owner) or as=service, not both")
	}
	kind, id, _ := strings.Cut(v, ":")
	if q := (run.Requester{Kind: run.RequesterKind(kind), ID: id}); !q.Valid() {
		return "", bad("owner is a requester's kind and id, as in member:<subject>")
	}
	return v, nil
}

func (s *Server) getSession(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	owner, err := ownerParam(r)
	if err != nil {
		return 0, nil, err
	}
	sess, events, err := s.Sessions.Read(ctx, c.Tenant, r.PathValue("id"))
	events = session.Shown(events)
	if err == nil && owner != "" && !sess.OwnedBy(owner) {
		err = session.ErrNotFound // another person's thread (#219)
	}
	if grant := readGrantHeader(r); err == nil && grant != "" {
		err = s.mayReadAnothers(ctx, r, c.Tenant, sess.Agent, owner, sess.ID, grant)
	} else if err == nil {
		err = s.mayRead(ctx, c.Tenant, sess.Agent, owner)
	}
	if err != nil {
		return 0, nil, err
	}
	if events == nil {
		events = []session.Event{}
	}
	return http.StatusOK, map[string]any{"session": sess, "events": events}, nil
}

// archiveSession archives a session, or puts it back (kiff/tafs#82): with an
// owner, only one that is theirs, or has no owner (#219). It answers the
// session as it now is.
func (s *Server) archiveSession(archived bool) func(context.Context, Caller, *http.Request) (int, any, error) {
	return func(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
		owner, err := ownerParam(r)
		if err != nil {
			return 0, nil, err
		}
		id := r.PathValue("id")
		sess, _, err := s.Sessions.Read(ctx, c.Tenant, id)
		if err == nil && owner != "" && !sess.OwnedBy(owner) {
			err = session.ErrNotFound
		}
		if err == nil {
			err = s.Sessions.SetArchived(ctx, c.Tenant, id, archived)
		}
		if err != nil {
			return 0, nil, err
		}
		sess.Archived = archived
		return http.StatusOK, map[string]any{"session": sess}, nil
	}
}

// deleteSession leaves "not found" to the Store's Delete, which also
// finishes a session whose deletion was cut short; a Read first would hide
// that one, and it could never be finished.
func (s *Server) deleteSession(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	// Deleted as the person, only their own thread, or as the service.
	owner, err := ownerParam(r)
	if err != nil {
		return 0, nil, err
	}
	if owner != "" {
		sess, _, err := s.Sessions.Read(ctx, c.Tenant, r.PathValue("id"))
		// Exactly theirs: a job's thread with no owner is shared history,
		// deleted only as the service.
		if err == nil && !sess.ContinuedBy(owner) {
			err = session.ErrNotFound // another person's thread, or a job's
		}
		if err != nil {
			return 0, nil, err
		}
	}
	if s.Files != nil {
		// Its files first: a deletion cut short is finished by repeating it.
		if err := s.Files.RemoveSession(ctx, c.Tenant, r.PathValue("id")); err != nil {
			return 0, nil, err
		}
	}
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
