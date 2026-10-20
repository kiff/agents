package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
)

// An agent's versions (kiff/tafs#107): each one kept, any one live, and a
// trial of one that sends nothing.
//
//	GET  /agents/{id}/versions              {"versions": [Version…]}, oldest first
//	POST /agents/{id}/versions              {definition, note, by, draft, revisions} → {"n"}; live unchanged
//	POST /agents/{id}/versions/{n}/pending  {draft?} → {"switching"}: the switch to n begins
//	POST /agents/{id}/versions/{n}/live     → {"live": n}; ends a switch to n
//	DELETE /agents/{id}/switching           → {"cancelled": true|false}
//	POST /agents/{id}/versions/{n}/trial    {input} → {"run"}, at once; it runs in the background
//	GET  /agents/{id}/trials/{run_id}       {"run", "writes"}: KIFF's decisions so far

// versionBody is what POST /agents/{id}/versions takes.
type versionBody struct {
	Definition registry.Definition `json:"definition"`
	Note       string              `json:"note"`
	By         string              `json:"by"`
	Draft      string              `json:"draft,omitempty"`
	Revisions  []string            `json:"revisions,omitempty"`
}

// MaxNote bounds a version's note: what changed, in plain words.
const MaxNote = 2000

func (s *Server) listVersions(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	vs, err := s.Agents.Versions(ctx, c.Tenant, r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	if vs == nil {
		vs = []registry.Version{}
	}
	out := map[string]any{"versions": vs}
	sw, on, err := s.Agents.Switching(ctx, c.Tenant, r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	if on {
		out["switching"] = sw
	}
	return http.StatusOK, out, nil
}

// putVersion stores the agent's next version, checked as PUT /agents/{id}
// checks a definition. What's live doesn't change.
func (s *Server) putVersion(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	var body versionBody
	if err := decode(r, &body); err != nil {
		return 0, nil, err
	}
	d, id := body.Definition, r.PathValue("id")
	if d.Tenant != "" && d.Tenant != c.Tenant {
		return 0, nil, bad("the tenant comes from the caller, not the body")
	}
	if d.ID != "" && d.ID != id {
		return 0, nil, bad("the id in the body is not the path's")
	}
	if len(body.Note) > MaxNote || len(body.By) > MaxNote {
		return 0, nil, bad("a note is at most %d bytes", MaxNote)
	}
	d.Tenant, d.ID, d.Version = c.Tenant, id, 0
	if err := d.Check(); err != nil {
		return 0, nil, badRequest{err}
	}
	if !slices.Contains(s.Gateways, d.Gateway) {
		return 0, nil, badRequest{registry.ErrGateway}
	}
	if err := s.checkSkills(ctx, c.Tenant, d.Skills); err != nil {
		return 0, nil, err
	}
	n, err := s.Agents.PutVersion(ctx, d, registry.Version{Note: body.Note, By: body.By, Draft: body.Draft, Revisions: body.Revisions})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]int{"n": n}, nil
}

// switchVersion makes version n live, forward or back, and ends a switch to
// n under way. While the agent switches to another version, ErrSwitching.
func (s *Server) switchVersion(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	n, err := versionNumber(r)
	if err != nil {
		return 0, nil, err
	}
	id := r.PathValue("id")
	sw, on, err := s.Agents.Switching(ctx, c.Tenant, id)
	if err != nil {
		return 0, nil, err
	}
	if on && sw.N != n {
		return 0, nil, registry.ErrSwitching
	}
	if err := s.Agents.Switch(ctx, c.Tenant, id, n); err != nil {
		return 0, nil, err
	}
	if on {
		if err := s.Agents.EndSwitch(ctx, c.Tenant, id); err != nil {
			return 0, nil, err
		}
	}
	return http.StatusOK, map[string]int{"live": n}, nil
}

// pendingBody is what POST /agents/{id}/versions/{n}/pending takes.
type pendingBody struct {
	// Draft is the KIFF draft the switch waits for; empty is the version's.
	Draft string `json:"draft"`
}

// pendingVersion begins the switch to version n (kiff/tafs#107). From then
// on nothing of the agent runs: no run starts, and a paused one waits on, so
// nothing runs with one version's definition under another's Cards while
// the owner confirms version n's Card revision. Trials still run. It begins
// only while no run of the agent is going (PENDING or RUNNING): one that
// started on the live definition must end first.
func (s *Server) pendingVersion(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	var body pendingBody
	if err := decode(r, &body); err != nil {
		return 0, nil, err
	}
	n, err := versionNumber(r)
	if err != nil {
		return 0, nil, err
	}
	id := r.PathValue("id")
	vs, err := s.Agents.Versions(ctx, c.Tenant, id)
	if err != nil {
		return 0, nil, err
	}
	i := slices.IndexFunc(vs, func(v registry.Version) bool { return v.N == n })
	if i < 0 {
		return 0, nil, registry.ErrNoVersion
	}
	if vs[i].Live {
		return 0, nil, conflict("that version is live already")
	}
	draft := body.Draft
	if draft == "" {
		draft = vs[i].Draft
	}
	at := s.now().UTC()
	sw, err := s.Agents.BeginSwitch(ctx, c.Tenant, id, registry.Switching{N: n, Draft: draft, At: at})
	if err != nil {
		return 0, nil, err
	}
	// Read once the mark is stored: a run claimed before it is seen here,
	// and one claimed after sees the mark (runner.Execute).
	runs, err := s.Runs.List(ctx, c.Tenant, id, maxRuns)
	if err != nil {
		return 0, nil, err
	}
	for _, rn := range runs {
		if rn.Active() && rn.Trigger != run.ByTrial {
			if sw.At.Equal(at) { // this request's mark: take it back
				if err := s.Agents.EndSwitch(ctx, c.Tenant, id); err != nil {
					return 0, nil, err
				}
			}
			return 0, nil, conflict("a run of this agent is going: stop it or let it finish, then switch")
		}
	}
	return http.StatusOK, map[string]any{"switching": sw}, nil
}

// cancelSwitch drops a switch under way: the live version stays, and the
// agent runs again.
func (s *Server) cancelSwitch(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	id := r.PathValue("id")
	if _, err := s.Agents.Get(ctx, c.Tenant, id); err != nil {
		return 0, nil, err
	}
	_, on, err := s.Agents.Switching(ctx, c.Tenant, id)
	if err != nil {
		return 0, nil, err
	}
	if on {
		if err := s.Agents.EndSwitch(ctx, c.Tenant, id); err != nil {
			return 0, nil, err
		}
	}
	return http.StatusOK, map[string]bool{"cancelled": on}, nil
}

func versionNumber(r *http.Request) (int, error) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 {
		return 0, registry.ErrNoVersion
	}
	return n, nil
}

// trialBody is what POST /agents/{id}/versions/{n}/trial takes.
type trialBody struct {
	Input string `json:"input"`
}

// startTrial starts a trial of version n in the background and answers at
// once: its writes are decided by KIFF against the version's draft and never
// sent, and its session is a trial's. GET /agents/{id}/trials/{run_id}
// reads how it went.
func (s *Server) startTrial(w http.ResponseWriter, r *http.Request) {
	c, err := s.caller(w, r)
	if err != nil {
		return
	}
	ctx := r.Context()
	var body trialBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Input == "" || len(body.Input) > MaxInput {
		s.fail(w, r, bad("the input is 1 to %d bytes", MaxInput))
		return
	}
	if s.Background == nil {
		s.fail(w, r, unavailable("this server doesn't run runs in the background"))
		return
	}
	n, err := versionNumber(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	id := r.PathValue("id")
	vs, err := s.Agents.Versions(ctx, c.Tenant, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	i := slices.IndexFunc(vs, func(v registry.Version) bool { return v.N == n })
	if i < 0 {
		s.fail(w, r, registry.ErrNoVersion)
		return
	}
	rn, err := s.start(ctx, Start{Tenant: c.Tenant, Agent: id, Trigger: run.ByTrial, Input: body.Input, Version: n, Draft: vs[i].Draft})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.toBackground(ctx, rn); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run": viewRun(rn)})
}

// trialWrite is one write of a trial, as the API shows it: the call as the
// taf made it, and what KIFF would have done with it.
type trialWrite struct {
	Tool      string          `json:"tool"`
	Title     string          `json:"title,omitempty"`
	Arguments json.RawMessage `json:"arguments"`
	Decision  string          `json:"decision"`
	Reason    string          `json:"reason,omitempty"`
}

// getTrial reads a trial: the run, and KIFF's decisions on its writes so far.
func (s *Server) getTrial(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	rn, err := s.agentRun(ctx, c, r)
	if err != nil {
		return 0, nil, err
	}
	if rn.Trigger != run.ByTrial {
		return 0, nil, run.ErrNotFound
	}
	_, events, err := s.Sessions.Read(ctx, c.Tenant, rn.Session)
	if err != nil {
		return 0, nil, err
	}
	writes := []trialWrite{}
	for _, e := range events {
		if e.Run != rn.ID || e.Kind != session.TrialWrite {
			continue
		}
		var b agent.TrialWriteBody
		if json.Unmarshal(e.Body, &b) != nil {
			continue
		}
		writes = append(writes, trialWrite{Tool: b.Tool, Title: b.Title, Arguments: b.Input, Decision: b.Decision, Reason: b.Reason})
	}
	return http.StatusOK, map[string]any{"run": viewRun(rn), "writes": writes}, nil
}
