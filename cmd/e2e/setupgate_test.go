package main

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kiff/agents/eval"
)

// A setup gate that failed is a setupGatesFailed, so the run can tell it from
// an error that stopped it: the taf's trial still runs after it (Eval
// 43666453786: "signs what was agreed" failed, and the Refunds trial never ran).
func TestAFailedSetupGateIsToldFromAnError(t *testing.T) {
	var log bytes.Buffer
	tt := &test{out: &redacting{w: &log, ci: true}, cfg: config{log: &log, judge: &eval.Judge{}}}
	err := tt.reportSetup([]eval.CheckResult{
		{Name: "concise", Grade: eval.Grade{Pass: true}},
		{Name: signsWhatWasAgreed, Grade: eval.Grade{Why: "the Card holds a day's total the owner didn't agree to"}},
	})
	var gates setupGatesFailed
	if !errors.As(err, &gates) || !slices.Contains(gates.names, signsWhatWasAgreed) {
		t.Fatalf("err %v (%T)", err, err)
	}
	if err.Error() != "the setup chat failed: "+signsWhatWasAgreed {
		t.Fatalf("the words changed: %q", err.Error())
	}
	// An advice or a skip fails nothing.
	if err := tt.reportSetup([]eval.CheckResult{
		{Name: "asks its questions together", Advisory: true},
		{Name: "narrows a managed sign-in", Grade: eval.Grade{Skipped: true}},
	}); err != nil {
		t.Fatalf("advice and skip: %v", err)
	}
	// An error that stopped the run isn't a gate.
	if errors.As(fmt.Errorf("judging: %w", errors.New("the judge didn't answer")), &gates) {
		t.Fatal("an error read as a gate")
	}
}

// A run that went on past a failed gate still fails for it, and for what failed
// after it; with no failed gate, it fails for what failed after, as before.
func TestARunPastAFailedGateStillFailsForIt(t *testing.T) {
	gate := setupGatesFailed{names: []string{signsWhatWasAgreed}}
	later := errors.New("the taf refunded nothing")

	if err := afterSetup(gate, later); err == nil ||
		!strings.Contains(err.Error(), signsWhatWasAgreed) || !strings.Contains(err.Error(), later.Error()) || !errors.Is(err, later) {
		t.Fatalf("both: %v", err)
	}
	var gates setupGatesFailed
	if err := afterSetup(gate, nil); err == nil || !errors.As(err, &gates) {
		t.Fatalf("the gate alone: %v", err)
	}
	if err := afterSetup(nil, later); err != later {
		t.Fatalf("no gate: %v", err)
	}
	if err := afterSetup(nil, nil); err != nil {
		t.Fatalf("nothing failed: %v", err)
	}
}
