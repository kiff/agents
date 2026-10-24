package runner_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/runner"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/tool"
)

// While an agent is halted nothing of it runs. No run
// starts, a trial neither (its reads still reach a vendor); one stored before
// the halt ends CANCELLED, never started; a paused one goes on only to record
// what KIFF answered for its held calls and ends before any model turn.
func TestNothingRunsWhileTheAgentIsHalted(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_refund", `{"order":42}`), say("Waiting for approval."), say("After the resume."))
	paused := f.pause(f.start(server.Start{Input: "refund order 42"}))
	f.answer(paused)
	stored := f.start(server.Start{Input: "a message before the halt"})
	if _, err := f.agents.Halt(ctx, "t1", "rae", f.clock.now()); err != nil {
		t.Fatal(err)
	}

	for _, s := range []server.Start{
		{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "go"},
		{Tenant: "t1", Agent: "rae", Trigger: run.BySchedule, Input: "go", Source: "morning"},
		{Tenant: "t1", Agent: "rae", Trigger: run.ByTrial, Input: "try"},
	} {
		if _, err := f.r.Start(ctx, s); !errors.Is(err, registry.ErrHalted) {
			t.Fatalf("a %s run started while halted: %v", s.Trigger, err)
		}
	}
	requests := len(f.model.requests())
	got, err := f.r.Execute(ctx, "t1", stored.ID, nil)
	if !errors.Is(err, registry.ErrHalted) || got.Status != run.Cancelled || got.Error != runner.FailedHalted {
		t.Fatalf("a run stored before the halt = %+v, %v; want it cancelled, not started", got, err)
	}
	// Its hold approved and not withdrawn (the halt could only mark it):
	// collecting it would have KIFF send the approved refund, so it isn't
	// collected. Each held call is recorded as not sent.
	collected := 0
	f.r.Collect = func(context.Context, agent.Agent, []run.Requirement) ([]agent.ToolResultBody, error) {
		collected++
		return []agent.ToolResultBody{{CallID: "c1", Tool: "stripe_refund", Result: tool.Result{Text: "refunded re_1"}}}, nil
	}
	var results []string
	got, err = f.r.Execute(ctx, "t1", paused.ID, func(e agent.Event) {
		if e.Kind == agent.CallResult && e.Result != nil {
			results = append(results, e.Result.Text)
		}
	})
	if err != nil || got.Status != run.Cancelled || got.Error != runner.FailedHalted || collected != 0 {
		t.Fatalf("a paused run while halted = %+v, %v (collected %d); want it cancelled, nothing collected", got, err, collected)
	}
	if len(results) != 1 || results[0] != runner.NotCollected {
		t.Fatalf("the held call's result = %q; want it said not sent", results)
	}
	if len(f.model.requests()) != requests {
		t.Fatal("the model was asked while the agent was halted")
	}

	// Resumed, new runs start; what came before a halt still never does.
	if _, err := f.agents.Resume(ctx, "t1", "rae"); err != nil {
		t.Fatal(err)
	}
	f.clock.add(time.Second)
	before := f.start(server.Start{Input: "made before the next halt"})
	f.clock.add(time.Second)
	if _, err := f.agents.Halt(ctx, "t1", "rae", f.clock.now()); err != nil { // halted again: before is from before it
		t.Fatal(err)
	}
	if _, err := f.agents.Resume(ctx, "t1", "rae"); err != nil {
		t.Fatal(err)
	}
	f.clock.add(time.Second)
	got, err = f.r.Execute(ctx, "t1", before.ID, nil)
	if !errors.Is(err, registry.ErrHalted) || got.Status != run.Cancelled {
		t.Fatalf("a run from before the halt, once resumed = %+v, %v; want it cancelled", got, err)
	}
	after := f.start(server.Start{Input: "after the resume"})
	if got, err := f.r.Execute(ctx, "t1", after.ID, nil); err != nil || got.Status != run.Completed {
		t.Fatalf("a run after the resume = %+v, %v", got, err)
	}
}

// Halting the tenant halts each of its agents; resuming the agent alone
// doesn't release it, resuming the tenant does.
func TestATenantsHaltHaltsItsAgents(t *testing.T) {
	f := newFixture(t, say("Done."))
	if _, err := f.agents.Halt(ctx, "t1", registry.Tenant, f.clock.now()); err != nil {
		t.Fatal(err)
	}
	start := server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "go"}
	if _, err := f.r.Start(ctx, start); !errors.Is(err, registry.ErrHalted) {
		t.Fatalf("a run started while its tenant is halted: %v", err)
	}
	if _, err := f.agents.Resume(ctx, "t1", "rae"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Start(ctx, start); !errors.Is(err, registry.ErrHalted) {
		t.Fatalf("an agent resumed on its own ran while its tenant is halted: %v", err)
	}
	if _, err := f.agents.Resume(ctx, "t1", registry.Tenant); err != nil {
		t.Fatal(err)
	}
	f.clock.add(time.Second)
	r := f.start(server.Start{Input: "go"})
	if got, err := f.r.Execute(ctx, "t1", r.ID, nil); err != nil || got.Status != run.Completed {
		t.Fatalf("a run once the tenant is resumed = %+v, %v", got, err)
	}
}
