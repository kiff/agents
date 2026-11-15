package main

import (
	"bytes"
	"context"
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

// scenario's step after the setup chat: a failed graded gate still lets the
// taf's trial run, and the run fails for both; an error grading stops it
// before the trial; with no judge the trial alone decides (PM: a test that
// drives the scenario's path, after Eval 43670959911 measured the trial).
func TestTheTrialRunsPastAFailedGateOnly(t *testing.T) {
	ctx := context.Background()
	gate := setupGatesFailed{names: []string{signsWhatWasAgreed}}
	later := errors.New("the taf's answer 4 says something was done, or not, that its record contradicts")

	for _, c := range []struct {
		name       string
		grade      func(context.Context) error
		trial      error
		wantTrial  bool
		wantGate   bool
		wantLater  bool
		wantLogged bool
	}{
		{"a gate fails, then the trial", func(context.Context) error { return gate }, later, true, true, true, true},
		{"a gate fails, the trial passes", func(context.Context) error { return gate }, nil, true, true, false, true},
		{"grading errs: no trial", func(context.Context) error { return errors.New("the judge didn't answer") }, nil, false, false, false, false},
		{"every gate passes", func(context.Context) error { return nil }, later, true, false, true, false},
		{"no judge", nil, later, true, false, true, false},
	} {
		var log bytes.Buffer
		ran := false
		err := runPastGates(ctx, &log, c.grade, func(context.Context) error { ran = true; return c.trial })
		var gates setupGatesFailed
		if ran != c.wantTrial {
			t.Errorf("%s: the trial ran %v", c.name, ran)
		}
		if got := errors.As(err, &gates); got != c.wantGate {
			t.Errorf("%s: the gate in the error: %v (%v)", c.name, got, err)
		}
		if got := errors.Is(err, later); got != c.wantLater {
			t.Errorf("%s: the trial's failure in the error: %v (%v)", c.name, got, err)
		}
		if got := strings.Contains(log.String(), "the taf is still tried"); got != c.wantLogged {
			t.Errorf("%s: logged %q", c.name, log.String())
		}
		if !c.wantTrial && err == nil {
			t.Errorf("%s: an error grading was dropped", c.name)
		}
		if c.wantTrial && !c.wantGate && !c.wantLater && err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}
