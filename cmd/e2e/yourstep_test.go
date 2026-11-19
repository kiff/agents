package main

import (
	"context"
	"io"
	"net/http"
	"testing"
)

// The setup suites' runner offers your_step as the Lambda does: a taf whose definition asks for it (kiff/tafs with
// TAFS_YOUR_STEP=on) builds here, rather than failing with "registry: this server doesn't offer your_step" (the Eval of
// 19 Nov, run 44583392323, the refunds step).
func TestTheRunnerOffersYourStep(t *testing.T) {
	tt := &test{tenant: "t1", hc: &http.Client{}, out: &redacting{w: io.Discard, ci: true}, cfg: config{log: io.Discard}}
	if err := tt.runtime(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if tt.rn.Builder.YourStep == nil {
		t.Fatal("the e2e runner doesn't offer your_step")
	}
	if got := tt.rn.Builder.YourStep("key-t1").Spec().Name; got != "your_step" {
		t.Fatalf("its tool is %q, want your_step", got)
	}
}
