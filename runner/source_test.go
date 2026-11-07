package runner_test

import (
	"context"
	"testing"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/tool"
)

// The run record says where the run's turns ran (model.Sourced), as the
// model's first reply said, and keeps it across a pause.
func TestARunRecordsWhereItsTurnsRan(t *testing.T) {
	ctx := context.Background()
	held := callRefund("c1", `{"order":42}`)
	held.Source, held.Region = model.SourceBedrock, "eu-west-1"
	waits := answer("The refund waits for your answer.")
	waits.Source, waits.Region = model.SourceBedrock, "eu-west-1"
	rn := newRunner(&replies{held, waits}, tool.Result{Text: "Held for the owner.", IsError: true, State: tool.StateHeld})
	r, _ := rn.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "Refund order 42."})
	r, err := rn.Execute(ctx, "t1", r.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != run.Paused || r.ModelSource != model.SourceBedrock || r.ModelRegion != "eu-west-1" {
		t.Fatalf("paused run: status %s, source %q, region %q", r.Status, r.ModelSource, r.ModelRegion)
	}
	// A run on the Claude API alone says so too.
	rn = newRunner(&replies{answer("Done.")}, tool.Result{})
	r, _ = rn.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "Hi."})
	if r, _ = rn.Execute(ctx, "t1", r.ID, nil); r.ModelSource != "" {
		// A model that runs in one place names none: the record stays empty, and the run on the Claude API.
		t.Fatalf("a plain model's run says %q", r.ModelSource)
	}
}
