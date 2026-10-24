package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/run"
)

// A halt stops an agent, or every agent of the
// tenant, until it is resumed: nothing of it starts or goes on (the runner
// checks), and what came before the halt never runs, even once resumed.
// Halting is stored first, then each active run is cancelled as a cancel
// does, so a run that starts meanwhile either is cancelled here or sees the
// halt once it is claimed. KIFF halts its side (its gateway refuses the
// agent's calls) on its own route; tafs cloud asks both.
//
//	GET  /agents/{id}/halt      the agent's halt, its tenant's over it
//	POST /agents/{id}/halt      halt the agent; its active runs are cancelled
//	POST /agents/{id}/resume    resume it (it stays halted while its tenant is)
//	GET  /halt                  the tenant's own halt
//	POST /halt                  halt every agent of the tenant, and any made meanwhile
//	POST /resume                resume the tenant (an agent halted on its own stays so)

// haltView is a halt as the API shows it.
type haltView struct {
	Agent string `json:"agent,omitempty"`
	// Halted: nothing of it runs. Own and Tenant say which halt holds.
	Halted bool       `json:"halted"`
	Own    bool       `json:"own"`
	Tenant bool       `json:"tenant"`
	At     *time.Time `json:"halted_at,omitempty"`
	// Stopped is how many runs this halt cancelled; Unstopped, those it
	// couldn't (they stop once claimed, as the runner refuses them).
	Stopped   int `json:"stopped"`
	Unstopped int `json:"unstopped,omitempty"`
}

func viewHalt(agent string, own, tenant registry.Halt) haltView {
	h := own.Over(tenant)
	v := haltView{Agent: agent, Halted: h.On, Own: own.On, Tenant: tenant.On}
	if !h.At.IsZero() {
		at := h.At
		v.At = &at
	}
	return v
}

func (s *Server) halts(ctx context.Context, tenant, agent string) (own, all registry.Halt, err error) {
	if agent != registry.Tenant {
		if own, err = s.Agents.Halted(ctx, tenant, agent); err != nil {
			return
		}
	}
	all, err = s.Agents.Halted(ctx, tenant, registry.Tenant)
	return
}

func (s *Server) getHalt(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	id := r.PathValue("id")
	if id != "" {
		if _, err := s.Agents.Get(ctx, c.Tenant, id); err != nil {
			return 0, nil, err
		}
	}
	own, all, err := s.halts(ctx, c.Tenant, id)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, viewHalt(id, own, all), nil
}

func (s *Server) halt(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	id := r.PathValue("id")
	if _, err := s.Agents.Halt(ctx, c.Tenant, id, s.now()); err != nil {
		return 0, nil, err
	}
	agents := []string{id}
	if id == registry.Tenant {
		defs, err := s.Agents.List(ctx, c.Tenant)
		if err != nil {
			return 0, nil, err
		}
		agents = agents[:0]
		for _, d := range defs {
			agents = append(agents, d.ID)
		}
	}
	stopped, unstopped := 0, 0
	for _, a := range agents {
		n, u, err := s.stopAll(ctx, c.Tenant, a)
		stopped, unstopped = stopped+n, unstopped+u
		if err != nil {
			return 0, nil, fmt.Errorf("cancelling the runs of %s: %w", a, err)
		}
	}
	own, all, err := s.halts(ctx, c.Tenant, id)
	if err != nil {
		return 0, nil, err
	}
	v := viewHalt(id, own, all)
	v.Stopped, v.Unstopped = stopped, unstopped
	return http.StatusOK, v, nil
}

func (s *Server) resume(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	id := r.PathValue("id")
	if _, err := s.Agents.Resume(ctx, c.Tenant, id); err != nil {
		return 0, nil, err
	}
	own, all, err := s.halts(ctx, c.Tenant, id)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, viewHalt(id, own, all), nil
}

// stopAll cancels every active run of the agent, as a cancel does: pending
// ones now, running ones at their next checkpoint, paused ones with their
// held calls withdrawn in KIFF. A paused run whose calls can't be withdrawn
// here is still marked to stop: KIFF refuses its held calls while the agent
// is halted, and once they are resolved it ends at its first checkpoint.
// It returns how many runs it cancelled, and how many it could only mark.
func (s *Server) stopAll(ctx context.Context, tenant, agent string) (stopped, marked int, err error) {
	runs, err := s.Runs.List(ctx, tenant, agent, 0)
	if err != nil {
		return 0, 0, err
	}
	for _, rn := range runs {
		if rn.Status.Final() {
			continue
		}
		_, err := s.cancel(ctx, rn)
		var c conflictError
		switch {
		case err == nil:
			stopped++
		case errors.As(err, &c) && rn.Status == run.Paused:
			if err := s.markStop(ctx, rn); err != nil {
				return stopped, marked, err
			}
			marked++
		case errors.As(err, &c): // it ended meanwhile
		default:
			return stopped, marked, err
		}
	}
	return stopped, marked, nil
}

// markStop sets a run's Cancel, reading it again while it changes.
func (s *Server) markStop(ctx context.Context, rn run.Run) error {
	for try := 1; ; try++ {
		if rn.Cancel || rn.Status.Final() {
			return nil
		}
		_, err := run.Change(ctx, s.Runs, rn, s.now(), func(n *run.Run) error { n.Cancel = true; return nil })
		if !errors.Is(err, run.ErrConflict) || try == cancelTries {
			return err
		}
		if rn, err = s.Runs.Get(ctx, rn.Tenant, rn.ID); err != nil {
			return err
		}
	}
}
