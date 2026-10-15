package eval

import (
	"context"
	"fmt"
	"time"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
)

// Policy is the keep rules for runs an eval grades: the default rules, but
// tool calls and their results are kept for retention like everything else.
// Under the default rules they are dropped when the run ends, and the checks
// that read them (Calls, Results) would have nothing to grade. Eval runs work
// on test data, so keeping what tools returned costs no customer's privacy.
func Policy(retention time.Duration) session.Policy {
	p := session.DefaultPolicy(retention)
	p[session.ToolCall] = session.Rule{Keep: session.Kept, Retention: retention}
	p[session.ToolResult] = session.Rule{Keep: session.Kept, Retention: retention}
	return p
}

// Load reads a run of this runtime for grading: its status from runs, and its
// session and only its own events from sessions. A run that hasn't ended is
// loaded as it is; its Status says so.
func Load(ctx context.Context, sessions session.Store, runs run.Runs, tenant, id string) (Run, error) {
	r, err := runs.Get(ctx, tenant, id)
	if err != nil {
		return Run{}, fmt.Errorf("eval: run %s: %w", id, err)
	}
	s, events, err := sessions.Read(ctx, tenant, r.Session)
	if err != nil {
		return Run{}, fmt.Errorf("eval: run %s: session %s: %w", id, r.Session, err)
	}
	own := make([]session.Event, 0, len(events))
	for _, e := range events {
		if e.Run == r.ID {
			own = append(own, e)
		}
	}
	return Run{Session: s, Status: r.Status, Events: own}, nil
}
