package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/kiff/agents/tool/deliverfile"
)

// A taf whose definition sets files (kiff/tafs's do, since its definitions
// turn on deliver_file) builds through this test's runtime for a run, with
// deliver_file in its tools, as on the Lambda. Without the runner's
// DeliverFile, its first build failed with registry.ErrNoFiles, and every
// Setup step with it.
func TestATafWithFilesBuildsWithDeliverFile(t *testing.T) {
	ctx := context.Background()
	def := filepath.Join(t.TempDir(), "refunds.json")
	if err := os.WriteFile(def, []byte(`{"instructions":"You are Refunds.","files":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tt := &test{cfg: config{tafDef: def, planque: "https://planque.test", log: io.Discard, maxCost: 2_000_000}, out: &redacting{w: io.Discard},
		tenant: "tnt_test", route: &routeTools{}}
	if err := tt.runtime(ctx, nil); err != nil {
		t.Fatal(err)
	}
	d, err := tt.tafDefinition("taf_refunds")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Files {
		t.Fatal("-taf's files: true wasn't read")
	}
	if err := tt.agents.Put(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := tt.keys.Create(ctx, tt.tenant, d.KeyRef, "kf_test"); err != nil {
		t.Fatal(err)
	}
	a, _, err := tt.rn.Builder.BuildRun(ctx, tt.tenant, d.ID, "run_1")
	if err != nil {
		t.Fatalf("a taf with files doesn't build for a run: %v", err)
	}
	var names []string
	for _, tl := range a.Tools {
		names = append(names, tl.Spec().Name)
	}
	found := false
	for _, n := range names {
		found = found || n == deliverfile.Name
	}
	if !found {
		t.Fatalf("the run's tools are %v, without %s", names, deliverfile.Name)
	}
}
