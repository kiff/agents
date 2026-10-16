package kiff_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/registry/registrytest"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/runner"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/kiff"
)

// collector is an agent whose tools come from the fake gateway.
func collector(t *testing.T) agent.Agent {
	t.Helper()
	srv, _ := gateway(t, false)
	tools, err := kiff.New(srv.URL, testKey, srv.Client()).Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return agent.Agent{Tools: tools}
}

func requirement(callID, op string) run.Requirement {
	return run.Requirement{CallID: callID, Tool: "stripe_refund", Input: json.RawMessage(`{"charge": "ch_big", "amount": 90000}`), Operation: op}
}

// Collect repeats each held call with its operation ID, and returns each
// outcome in order: an approved call's result, an expired hold's refusal.
func TestCollectRepeatsEachCallByItsOperation(t *testing.T) {
	a := collector(t)
	got, err := kiff.Collect(context.Background(), a, []run.Requirement{requirement("c1", "op-approved"), requirement("c2", "op-expired")})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].CallID != "c1" || got[0].Tool != "stripe_refund" || got[0].Text != "refunded re_1" || got[0].IsError || got[0].Held() {
		t.Fatalf("collected %+v", got)
	}
	if got[1].CallID != "c2" || !got[1].IsError || got[1].State != "refused" {
		t.Fatalf("an expired hold collected as %+v", got[1])
	}
	// The call was repeated as the model made it, with its operation ID.
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0]), &args); err != nil {
		t.Fatal(err)
	}
	if args["kiff_operation_id"] != "op-approved" || args["charge"] != "ch_big" || args["amount"] != float64(90000) || len(args) != 3 {
		t.Fatalf("repeated with %v", args)
	}
}

// A call with no final outcome yet, still held, or still being decided or
// sent, is ErrNotFinal, and nothing is returned: the run stays paused.
func TestCollectLeavesACallWithNoOutcome(t *testing.T) {
	a := collector(t)
	for _, op := range []string{"op-waiting", "op-deciding", "op-forwarding", "op-held-no-retry"} {
		got, err := kiff.Collect(context.Background(), a, []run.Requirement{requirement("c1", "op-approved"), requirement("c2", op)})
		if !errors.Is(err, kiff.ErrNotFinal) || got != nil {
			t.Fatalf("%s: got %+v, %v; want ErrNotFinal", op, got, err)
		}
	}
}

// Collect refuses what it can't collect, before calling the gateway: a call
// with no operation ID, or a tool the agent doesn't have. A gateway error is
// returned as an error.
func TestCollectRefuses(t *testing.T) {
	a := collector(t)
	other := requirement("c1", "op-approved")
	other.Tool = "stripe_delete"
	for name, q := range map[string]run.Requirement{"no operation": requirement("c1", ""), "unknown tool": other} {
		calls = nil
		if _, err := kiff.Collect(context.Background(), a, []run.Requirement{q}); err == nil || len(calls) != 0 {
			t.Fatalf("%s: err = %v, %d calls", name, err, len(calls))
		}
	}
	if _, err := kiff.Collect(context.Background(), a, []run.Requirement{requirement("c1", "op-error")}); err == nil || errors.Is(err, kiff.ErrNotFinal) {
		t.Fatalf("a gateway error: err = %v", err)
	}
}

// replies is a model that answers each turn with the next reply.
type replies []model.Reply

func (r *replies) Turn(context.Context, model.Request, func(string)) (model.Reply, error) {
	next := (*r)[0]
	*r = (*r)[1:]
	return next, nil
}

type secrets struct{}

func (secrets) Key(context.Context, string, string) (string, error) { return testKey, nil }

// heldRun is a runner on memory stores whose agent's tools come from the fake
// gateway and are collected with kiff.Collect, and a run of it paused on a
// refund called with input, with KIFF's answer recorded on it.
func heldRun(t *testing.T, input string) (*runner.Runner, *replies, run.Run) {
	t.Helper()
	ctx := context.Background()
	srv, _ := gateway(t, false)
	m := &replies{
		{Blocks: []model.Block{{Kind: model.ToolUse, CallID: "c1", Tool: "stripe_refund", Input: json.RawMessage(input)}}, Stop: model.CallTools},
		{Blocks: []model.Block{{Kind: model.Text, Text: "The refund waits for your answer."}}, Stop: model.EndTurn},
		{Blocks: []model.Block{{Kind: model.Text, Text: "Refunded ch_big."}}, Stop: model.EndTurn},
	}
	agents := memory.NewAgents()
	d := registrytest.Definition("t1", "rae") // its gateway, https://gateway.test/mcp, is the fake
	if err := agents.Put(ctx, d); err != nil {
		t.Fatal(err)
	}
	runs := memory.NewRuns()
	rn := &runner.Runner{Runs: runs, Sessions: memory.New(), Collect: kiff.Collect,
		Builder: &registry.Builder{Agents: agents, Secrets: secrets{}, Model: m, Gateways: []string{d.Gateway},
			Connect: func(ctx context.Context, _, key string) ([]tool.Tool, error) {
				return kiff.New(srv.URL, key, srv.Client()).Tools(ctx)
			}}}
	r, err := rn.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "Refund ch_big."})
	if err != nil {
		t.Fatal(err)
	}
	if r, err = rn.Execute(ctx, "t1", r.ID, nil); err != nil || r.Status != run.Paused || len(r.Requirements) != 1 {
		t.Fatalf("paused %+v, %v", r, err)
	}
	// KIFF's hold_answered alert records the answer (the alerts route, next).
	r, err = run.Change(ctx, runs, r, time.Now(), func(n *run.Run) error {
		n.Requirements[0].Answered = time.Now()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return rn, m, r
}

// End to end through the gateway: a held refund pauses the run with its
// operation; once KIFF's answer is recorded, Execute collects the outcome by
// repeating the call, and the run carries on to its answer.
func TestAHeldRunContinuesThroughTheGateway(t *testing.T) {
	rn, _, r := heldRun(t, `{"charge":"ch_big"}`)
	if r.Requirements[0].Operation != "op-refund-42" {
		t.Fatalf("requirement %+v", r.Requirements[0])
	}
	r, err := rn.Execute(context.Background(), "t1", r.ID, nil)
	if err != nil || r.Status != run.Completed || r.Output != "Refunded ch_big." {
		t.Fatalf("continued %+v, %v", r, err)
	}
}

// A call KIFF is still deciding when the run is collected leaves the run
// paused, its requirement intact and no model turn taken; the next Execute
// collects the outcome and the run carries on.
func TestACallStillBeingDecidedLeavesTheRunPaused(t *testing.T) {
	rn, m, r := heldRun(t, `{"charge":"ch_big","slow":true}`)
	ctx := context.Background()
	got, err := rn.Execute(ctx, "t1", r.ID, nil)
	if !errors.Is(err, kiff.ErrNotFinal) || got.Status != run.Paused || len(got.Requirements) != 1 || got.Requirements[0].Operation != "op-slow" {
		t.Fatalf("got %+v, %v; want it paused again", got, err)
	}
	if len(*m) != 1 {
		t.Fatalf("%d model replies left, want 1: no turn while KIFF decides", len(*m))
	}
	got, err = rn.Execute(ctx, "t1", r.ID, nil)
	if err != nil || got.Status != run.Completed || got.Output != "Refunded ch_big." {
		t.Fatalf("collected later: %+v, %v", got, err)
	}
}

// A held call the model made without kiff_operation_id is collected by
// KIFF's own invocation key, which the gateway accepts in its place: the run
// doesn't stay paused for good.
func TestACallMadeWithoutAnOperationIsCollectedByItsKey(t *testing.T) {
	rn, _, r := heldRun(t, `{"charge":"ch_big","keyless":true}`)
	if r.Requirements[0].Operation != "tc-77" {
		t.Fatalf("requirement %+v, want operation tc-77", r.Requirements[0])
	}
	r, err := rn.Execute(context.Background(), "t1", r.ID, nil)
	if err != nil || r.Status != run.Completed || r.Output != "Refunded ch_big." {
		t.Fatalf("continued %+v, %v", r, err)
	}
	var args map[string]any
	_ = json.Unmarshal([]byte(calls[len(calls)-1]), &args)
	if args["kiff_operation_id"] != "tc-77" {
		t.Fatalf("collected with %v", args)
	}
}
