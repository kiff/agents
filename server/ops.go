package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
)

// The operators' routes: read-only, across tenants, for the people building
// and running the agents (the console reads them). They never take a
// tenant's caller: Server.Operator verifies an operator, and every other
// route keeps its tenant rule unchanged. They change nothing.

// opsRunView is a run as operators see it: a run's view, with its tenant.
type opsRunView struct {
	Tenant string `json:"tenant"`
	runView
}

func viewOpsRun(r run.Run) opsRunView { return opsRunView{Tenant: r.Tenant, runView: viewRun(r)} }

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
	op("GET /ops/tenants/{tenant}/runs/{run_id}/events", s.opsRunEvents)
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

// opsRunEvents is what a run of a tenant recorded, as kept: what the keep
// rules dropped is gone, and isn't shown.
func (s *Server) opsRunEvents(ctx context.Context, r *http.Request) (int, any, error) {
	tenant := r.PathValue("tenant")
	rn, err := s.Runs.Get(ctx, tenant, r.PathValue("run_id"))
	if err != nil {
		return 0, nil, err
	}
	_, events, err := s.Sessions.ReadRun(ctx, tenant, rn.Session, rn.ID)
	if err != nil && !errors.Is(err, session.ErrNotFound) {
		return 0, nil, err
	}
	if events == nil {
		events = []session.Event{}
	}
	return http.StatusOK, map[string]any{"run": viewOpsRun(rn), "events": events}, nil
}
