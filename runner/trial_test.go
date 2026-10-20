package runner_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/registry/registrytest"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool"
)

// kiff/tafs#107: a trial runs the version it tries, not the live one, its
// tools marked with the trial's draft, and keeps KIFF's decision on each
// write after its tool calls are gone.
func TestATrialRunsItsVersionAndRecordsItsWrites(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_refund", `{"charge":"ch_1","amount":30000}`), say("KIFF would hold that refund."))
	v2 := registrytest.Definition("t1", "rae")
	v2.Instructions = "Version two: read the charge first."
	n, err := f.agents.PutVersion(ctx, v2, registry.Version{Note: "reads first", Draft: "drf_2"})
	if err != nil || n != 2 {
		t.Fatalf("PutVersion = %d, %v", n, err)
	}
	f.refund.result = tool.Result{Text: "Trial: KIFF would hold this call. Nothing was sent.",
		Trial: &tool.TrialDecision{Decision: "held", Reason: "above the limit for one call", Title: "Create refund"}}
	r := f.start(server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByTrial, Input: "refund ch_1", Version: 2, Draft: "drf_2"})
	if r.AgentVersion != 2 || r.Draft != "drf_2" {
		t.Fatalf("stored trial %+v", r)
	}
	got, err := f.r.Execute(ctx, "t1", r.ID, nil)
	if err != nil || got.Status != run.Completed {
		t.Fatalf("trial ended %+v, %v", got, err)
	}
	if reqs := f.model.requests(); len(reqs) == 0 || !strings.Contains(reqs[0].System, "Version two: read the charge first.") {
		t.Fatal("the trial didn't run the version it tries")
	}
	// The mark names the trial's run, so KIFF counts its earlier writes.
	if len(f.refund.trials) != 1 || f.refund.trials[0] != (run.Trial{Draft: "drf_2", Run: r.ID}) {
		t.Fatalf("the gateway call's trial mark = %+v", f.refund.trials)
	}
	// Tool calls go at the run's end; the trial's writes stay.
	if kinds := f.events(r); !slices.Contains(kinds, session.TrialWrite) || slices.Contains(kinds, session.ToolCall) {
		t.Fatalf("recorded %v, want the trial write kept and the tool call gone", kinds)
	}
	if sess, _, err := f.store.Read(ctx, "t1", r.Session); err != nil || sess.Kind != session.KindTrial {
		t.Fatalf("the trial's session = %+v, %v; want a trial's", sess, err)
	}
	// The live version is unchanged by it.
	if live, _ := f.agents.Get(ctx, "t1", "rae"); live.Version != 1 {
		t.Fatalf("live = v%d after a trial, want v1", live.Version)
	}
}

// An ordinary run records the live version it ran, and carries no draft.
func TestARunRecordsItsVersion(t *testing.T) {
	f := newFixture(t, say("Done."))
	if _, err := f.agents.PutVersion(ctx, registrytest.Definition("t1", "rae"), registry.Version{}); err != nil {
		t.Fatal(err)
	}
	if err := f.agents.Switch(ctx, "t1", "rae", 2); err != nil {
		t.Fatal(err)
	}
	r := f.start(server.Start{Input: "go"})
	if r.AgentVersion != 2 || r.Draft != "" {
		t.Fatalf("run %+v, want version 2 and no draft", r)
	}
	if _, err := f.r.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "go", Version: 1}); err == nil {
		t.Fatal("an ordinary run started a version of its choosing")
	}
}
