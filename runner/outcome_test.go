package runner_test

import (
	"context"
	"encoding/json"
	"testing"

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
