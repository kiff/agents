package runner_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/outcome"
)

// A run started with outcomes holds request_outcome, and the run keeps the
// outcome it accepted: kiff/agents' record, never the model's words.
func TestARunKeepsTheOutcomeItsToolAccepted(t *testing.T) {
	ctx := context.Background()
	m := &replies{
		{Blocks: []model.Block{{Kind: model.ToolUse, CallID: "c1", Tool: outcome.Name, Input: json.RawMessage(`{"value":"refused"}`)}}, Stop: model.CallTools},
		answer("Outcome: refunded"),
	}
	rn := newRunner(m, tool.Result{Text: "refunded re_1"})
	rn.Builder.Outcome = func(ctx context.Context, tenant, agent, runID string) (tool.Tool, error) {
		r, err := rn.Runs.Get(ctx, tenant, runID)
		if err != nil || len(r.Outcomes) == 0 {
			return nil, err
		}
		return outcome.New(r.Outcomes), nil
	}
	r, err := rn.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "Check A-1047.", Outcomes: []string{"refunded", "refused"}})
	if err != nil {
		t.Fatal(err)
	}
	if r, err = rn.Execute(ctx, "t1", r.ID, nil); err != nil {
		t.Fatal(err)
	}
	if r.Status != run.Completed || r.Outcome != "refused" {
		t.Fatalf("run %s, outcome %q: want COMPLETED, refused (the tool's, not the words')", r.Status, r.Outcome)
	}
	got, _ := rn.Runs.Get(ctx, "t1", r.ID)
	if got.Outcome != "refused" || len(got.Outcomes) != 2 {
		t.Fatalf("stored: outcome %q, outcomes %v", got.Outcome, got.Outcomes)
	}
}

// The outcome a run answered before it paused is kept at the pause: the run
// goes on later, after the owner's answer, and ends with it, though the part
// after the pause answers none.
func TestTheOutcomeOutlivesAPause(t *testing.T) {
	ctx := context.Background()
	m := &replies{
		{Blocks: []model.Block{{Kind: model.ToolUse, CallID: "c1", Tool: outcome.Name, Input: json.RawMessage(`{"value":"refunded"}`)}}, Stop: model.CallTools},
		callRefund("c2", `{"charge":"ch_1"}`),
		answer("Refunded A-1047 once the owner approved."),
		answer("Refunded A-1047 once the owner approved."),
	}
	rn := newRunner(m, tool.Result{Text: "KIFF holds this call for the owner", State: tool.StateHeld,
		Hold: &tool.Hold{Operation: "op_1", Exception: "exc_1", Expires: time.Now().Add(10 * time.Minute)}})
	rn.Builder.Outcome = func(ctx context.Context, tenant, agent, runID string) (tool.Tool, error) {
		r, err := rn.Runs.Get(ctx, tenant, runID)
		if err != nil || len(r.Outcomes) == 0 {
			return nil, err
		}
		return outcome.New(r.Outcomes), nil
	}
	r, err := rn.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "Refund A-1047.", Outcomes: []string{"refunded", "refused"}})
	if err != nil {
		t.Fatal(err)
	}
	paused, err := rn.Execute(ctx, "t1", r.ID, nil)
	if err != nil || paused.Status != run.Paused {
		t.Fatalf("paused: %s, %v", paused.Status, err)
	}
	if got, _ := rn.Runs.Get(ctx, "t1", r.ID); got.Outcome != "refunded" {
		t.Fatalf("at the pause: outcome %q, want refunded", got.Outcome)
	}
	// KIFF went through with the call once the owner approved it.
	rn.Collect = func(_ context.Context, _ agent.Agent, held []run.Requirement) ([]agent.ToolResultBody, error) {
		var out []agent.ToolResultBody
		for _, q := range held {
			out = append(out, agent.ToolResultBody{CallID: q.CallID, Tool: q.Tool, Result: tool.Result{Text: "refunded re_1", State: "forwarded"}})
		}
		return out, nil
	}
	if _, err := run.Answer(ctx, rn.Runs, "t1", "exc_1", "approved", time.Now()); err != nil {
		t.Fatal(err)
	}
	done, err := rn.Execute(ctx, "t1", r.ID, nil)
	if err != nil || done.Status != run.Completed {
		t.Fatalf("after the answer: %s, %v", done.Status, err)
	}
	if got, _ := rn.Runs.Get(ctx, "t1", r.ID); got.Outcome != "refunded" {
		t.Fatalf("ended: outcome %q, want refunded, answered before the pause", got.Outcome)
	}
}
