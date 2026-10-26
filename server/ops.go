package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/kiff/agents/run"
)

// The operators' routes: read-only, across tenants, for the people building
// and running the agents (the console reads them). They never take a
// tenant's caller: Server.Operator verifies an operator, and every other
// route keeps its tenant rule unchanged. They change nothing, and they show
// a run's state, never its content: no route reads a session's events.

// opsRunView is a run as operators see it: what state it is in and what it
// cost, never what anyone wrote or what it said. No input, output, error,
// steering, requester or held call's input: a person's content isn't an
// operator's to read.
type opsRunView struct {
	Tenant       string      `json:"tenant"`
	ID           string      `json:"id"`
	Agent        string      `json:"agent"`
	Trigger      run.Trigger `json:"trigger"`
	Status       run.Status  `json:"status"`
	Cost         int64       `json:"cost_micro_usd"`
	Turns        int         `json:"turns"`
	Held         []string    `json:"held,omitempty"` // the held calls' tools, by name
	AgentVersion int         `json:"agent_version,omitempty"`
	Created      time.Time   `json:"created"`
	Updated      time.Time   `json:"updated"`
}

func viewOpsRun(r run.Run) opsRunView {
	v := opsRunView{Tenant: r.Tenant, ID: r.ID, Agent: r.Agent, Trigger: r.Trigger, Status: r.Status,
		Cost: r.Cost, Turns: r.Turns, AgentVersion: r.AgentVersion, Created: r.Created, Updated: r.Updated}
	for _, q := range r.Requirements {
		v.Held = append(v.Held, q.Tool)
	}
	return v
}

// opsRoutes adds the operators' routes to mux, when an Operator check is set.
func (s *Server) opsRoutes(mux *http.ServeMux) {
	if s.Operator == nil {
		return
	}
	op := func(pattern string, h func(context.Context, *http.Request) (int, any, error)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if err := s.Operator(r); err != nil {
				s.fail(w, r, fmt.Errorf("%w: %w", ErrUnauthorized, err))
				return
			}
			status, body, err := h(r.Context(), r)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			writeJSON(w, status, body)
		})
	}
	op("GET /ops/runs", s.opsRuns(s.Runs.ListAll))
	op("GET /ops/runs/paused", s.opsRuns(s.Runs.ListPaused))
	op("GET /ops/tenants/{tenant}/runs/{run_id}", s.opsRun)
}

// opsRuns lists runs across tenants, newest first, at most limit.
func (s *Server) opsRuns(list func(ctx context.Context, limit int) ([]run.Run, error)) func(context.Context, *http.Request) (int, any, error) {
	return func(ctx context.Context, r *http.Request) (int, any, error) {
		limit := defaultRuns
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > maxRuns {
				return 0, nil, bad("limit is 1 to %d", maxRuns)
			}
			limit = n
		}
		runs, err := list(ctx, limit)
		if err != nil {
			return 0, nil, err
		}
		out := make([]opsRunView, len(runs))
		for i, rn := range runs {
			out[i] = viewOpsRun(rn)
		}
		return http.StatusOK, map[string]any{"runs": out}, nil
	}
}

// opsRun is one run of a tenant.
func (s *Server) opsRun(ctx context.Context, r *http.Request) (int, any, error) {
	rn, err := s.Runs.Get(ctx, r.PathValue("tenant"), r.PathValue("run_id"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, viewOpsRun(rn), nil
}
