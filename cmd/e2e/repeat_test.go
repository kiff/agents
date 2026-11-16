package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kiff/agents/eval"
)

func passCheck(name string) eval.CheckResult {
	return eval.CheckResult{Name: name, Grade: eval.Grade{Pass: true}}
}
func missCheck(name string) eval.CheckResult {
	return eval.CheckResult{Name: name, Grade: eval.Grade{Why: "no"}}
}

func TestEachRunWritesItsOwnTranscript(t *testing.T) {
	for in, want := range map[[2]any]string{
		{"setup.md", 1}: "setup.md", {"setup.md", 2}: "setup-2.md", {"replay-intros.md", 3}: "replay-intros-3.md",
		{"out", 2}: "out-2", {"", 2}: "",
	} {
		if got := runTranscript(in[0].(string), in[1].(int)); got != want {
			t.Errorf("%v: %q, want %q", in, got, want)
		}
	}
	got := withoutRepeat([]string{"-setup", "s.json", "-repeat", "3", "-transcript", "setup.md", "-grade", "-repeat=3", "--transcript=x.md"}, "setup-2.md")
	if want := []string{"-setup", "s.json", "-grade", "-transcript", "setup-2.md"}; !slices.Equal(got, want) {
		t.Fatalf("args: %q", got)
	}
}

// A safety gate holds in every run that graded it; a quality gate in most;
// skipped and advisory checks aren't held.
func TestTheBarHoldsSafetyInEveryRunAndQualityInMost(t *testing.T) {
	quality := eval.QualityGates[0]
	safety := eval.SafetyGates[0]
	advice := eval.CheckResult{Name: "concise", Advisory: true}
	skipped := eval.CheckResult{Name: "narrows a managed sign-in", Grade: eval.Grade{Skipped: true}}
	var log bytes.Buffer
	failed := holdToBar([][]eval.CheckResult{
		{passCheck(safety), missCheck(quality), advice, skipped},
		{passCheck(safety), passCheck(quality), advice, skipped},
		{passCheck(safety), passCheck(quality)},
	}, "setup", &log)
	if len(failed) != 0 {
		t.Fatalf("2 of 3 on a quality gate failed: %v\n%s", failed, log.String())
	}
	if !strings.Contains(log.String(), "warn  "+quality+": 2 of 3 runs (experience, in the definition)") {
		t.Fatalf("log: %s", log.String())
	}
	failed = holdToBar([][]eval.CheckResult{{passCheck(safety)}, {missCheck(safety)}, {passCheck(safety)}}, "setup", &log)
	if !slices.Equal(failed, []string{safety}) {
		t.Fatalf("a safety gate missed once held: %v", failed)
	}
	failed = holdToBar([][]eval.CheckResult{{missCheck(quality)}, {missCheck(quality)}, {passCheck(quality)}}, "setup", &log)
	if !slices.Equal(failed, []string{quality}) {
		t.Fatalf("a quality gate passing 1 of 3 held: %v", failed)
	}
	// A check in neither list is held in every run.
	failed = holdToBar([][]eval.CheckResult{{passCheck("a new gate")}, {missCheck("a new gate")}, {passCheck("a new gate")}}, "setup", &log)
	if !slices.Equal(failed, []string{"a new gate"}) {
		t.Fatalf("an unnamed check passed on most runs: %v", failed)
	}
}

// Each run reports what it graded; a run that reached no result fails
// aResult; a run that broke for anything else fails the repeat, whatever the
// tally says.
func TestRepeatRunsTallyTheirGradesAndFailABrokenRun(t *testing.T) {
	quality := eval.QualityGates[0]
	runs := []func(context.Context) error{
		func(ctx context.Context) error {
			record(ctx, []eval.CheckResult{missCheck(quality)})
			return setupGatesFailed{names: []string{quality}}
		},
		func(ctx context.Context) error { record(ctx, []eval.CheckResult{passCheck(quality)}); return nil },
		func(ctx context.Context) error { record(ctx, []eval.CheckResult{passCheck(quality)}); return nil },
	}
	var seen [][]string
	once := func(rs []func(context.Context) error) func(context.Context, []string) error {
		i := 0
		return func(ctx context.Context, args []string) error {
			seen = append(seen, args)
			i++
			return rs[i-1](ctx)
		}
	}
	var log bytes.Buffer
	if err := repeatRuns(context.Background(), 3, []string{"-repeat", "3", "-transcript", "setup.md"}, "setup.md", "setup", &log, once(runs)); err != nil {
		t.Fatalf("a quality gate missed once failed the repeat: %v\n%s", err, log.String())
	}
	if len(seen) != 3 || !slices.Equal(seen[2], []string{"-transcript", "setup-3.md"}) {
		t.Fatalf("the runs' args: %q", seen)
	}

	noResultTwice := []func(context.Context) error{
		func(context.Context) error { return noResult{turns: 14} },
		func(ctx context.Context) error { record(ctx, []eval.CheckResult{passCheck(quality)}); return nil },
		func(context.Context) error {
			return errors.Join(setupGatesFailed{names: []string{"x"}}, noResult{turns: 14})
		},
	}
	err := repeatRuns(context.Background(), 3, nil, "", "setup", &log, once(noResultTwice))
	if err == nil || !strings.Contains(err.Error(), aResult) {
		t.Fatalf("no result in two runs: %v", err)
	}

	broke := []func(context.Context) error{
		func(ctx context.Context) error { record(ctx, []eval.CheckResult{passCheck(quality)}); return nil },
		func(ctx context.Context) error {
			record(ctx, []eval.CheckResult{passCheck(quality)})
			return errors.Join(setupGatesFailed{names: []string{quality}}, errors.New("the taf's trial: KIFF answered 500"))
		},
		func(ctx context.Context) error { record(ctx, []eval.CheckResult{passCheck(quality)}); return nil },
	}
	err = repeatRuns(context.Background(), 3, nil, "", "setup", &log, once(broke))
	if err == nil || !strings.Contains(err.Error(), "1 of 3 runs broke") || !strings.Contains(err.Error(), "KIFF answered 500") {
		t.Fatalf("a broken run passed the repeat: %v", err)
	}
}

// Alone, a run reports to no tally.
func TestARunAloneHasNoTally(t *testing.T) {
	record(context.Background(), []eval.CheckResult{passCheck("x")})
	if tallyFrom(context.Background()) != nil {
		t.Fatal("a tally without -repeat")
	}
}

// A run that reached a result passes aResult: no result in 1 of 3 runs holds
// with a warning when aResult is a quality gate, and in 2 of 3 fails.
func TestEveryRunCountsTowardAResult(t *testing.T) {
	once := func(misses ...bool) func(context.Context, []string) error {
		i := 0
		return func(ctx context.Context, _ []string) error {
			i++
			if misses[i-1] {
				return noResult{turns: 14}
			}
			return nil
		}
	}
	var log bytes.Buffer
	err := repeatRuns(context.Background(), 3, nil, "", "setup", &log, once(true, false, false))
	if eval.QualityGate(aResult) {
		if err != nil || !strings.Contains(log.String(), "warn  "+aResult+": 2 of 3 runs (experience, in the definition)") {
			t.Fatalf("no result in 1 of 3 runs: %v\n%s", err, log.String())
		}
	} else if err == nil {
		t.Fatal("no result in 1 of 3 held as a safety gate")
	}
	log.Reset()
	err = repeatRuns(context.Background(), 3, nil, "", "setup", &log, once(true, false, true))
	if err == nil || !strings.Contains(log.String(), "FAIL  "+aResult+": 1 of 3 runs") {
		t.Fatalf("no result in 2 of 3 runs held: %v\n%s", err, log.String())
	}
}

// A run's case is named as eval.yml's transcripts name it, so a known miss
// names exactly one.
func TestARunNamesItsCaseAsTheTranscriptsDo(t *testing.T) {
	for want, got := range map[string]string{
		"setup":         e2eCase(false, false, "", ""),
		"desk":          e2eCase(true, false, "", ""),
		"invoices":      e2eCase(false, true, "", ""),
		"replay-intros": e2eCase(false, false, "intros", ""),
		"plan-sheets":   e2eCase(false, false, "", "sheets"),
	} {
		if got != want {
			t.Errorf("%s, want %s", got, want)
		}
	}
}
