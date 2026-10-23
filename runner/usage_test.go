package runner_test

import (
	"context"
	"testing"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
)

// A counted chat adds what it cost to its period each time it pauses or ends
// (kiff/tafs#193): a paused run that continues is counted for each part, and
// the sum is the run's whole cost, never more.
func TestACountedRunAddsItsCostOnce(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_refund", `{"order":42}`), say("Waiting for approval."))
	use := memory.NewUsage()
	f.r.Usage = use
	r := f.start(server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "refund order 42", Meter: "2026-10-20"})
	if got := f.get(r.ID); got.Meter != "2026-10-20" {
		t.Fatalf("stored meter %q", got.Meter)
	}
	paused := f.pause(r)
	if p, _ := use.Get(ctx, "t1", "2026-10-20"); p.Spent != paused.Cost || paused.Cost == 0 {
		t.Fatalf("after the pause: spent %d, the run cost %d", p.Spent, paused.Cost)
	}

	fresh := f.newRunner()
	fresh.Usage = use
	fresh.Collect = func(_ context.Context, a agent.Agent, held []run.Requirement) ([]agent.ToolResultBody, error) {
		return []agent.ToolResultBody{{CallID: "c1", Tool: "stripe_refund", Result: tool.Result{Text: "refunded"}}}, nil
	}
	f.answer(r)
	f.model.add(say("Refunded order 42."))
	got, err := fresh.Execute(ctx, "t1", r.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cost <= paused.Cost {
		t.Fatalf("the continued run cost %d, paused %d", got.Cost, paused.Cost)
	}
	if p, _ := use.Get(ctx, "t1", "2026-10-20"); p.Spent != got.Cost {
		t.Fatalf("spent %d, want the run's whole cost %d", p.Spent, got.Cost)
	}
}

// A run that wasn't started with a period isn't counted: scheduled work, and
// chats tafs cloud doesn't meter.
func TestARunWithNoMeterIsntCounted(t *testing.T) {
	f := newFixture(t, say("Done."))
	use := memory.NewUsage()
	f.r.Usage = use
	r := f.start(server.Start{Input: "hello"})
	got, err := f.r.Execute(ctx, "t1", r.ID, nil)
	if err != nil || got.Cost == 0 {
		t.Fatalf("ran to %+v, %v", got, err)
	}
	if p, _ := use.Get(ctx, "t1", ""); p.Spent != 0 {
		t.Fatalf("counted %d", p.Spent)
	}
}

// Where a run is announced is stored with it when it starts (kiff/tafs#206).
func TestStartStoresTheRunsNotify(t *testing.T) {
	f := newFixture(t, say("Done."))
	want := run.Notify{URL: "https://tafs.test/v1/runs/ended", Ref: "ref-0123456789abcdef"}
	r := f.start(server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "refund order 42", Notify: want})
	if got := f.get(r.ID); got.Notify != want {
		t.Fatalf("stored notify %+v", got.Notify)
	}
}
