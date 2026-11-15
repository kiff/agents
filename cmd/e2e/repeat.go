package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/kiff/agents/eval"
)

// -repeat N runs the same case N times, one after another, each a run of its
// own (its own taf, cleaned up after it, and its own transcript: setup.md,
// setup-2.md, setup-3.md), and then holds the graded checks to the Eval's bar
// (eval/gates.go): a safety gate passes in every run that graded it, a
// quality gate in most of them. One sample of a fourteen-turn chat was the
// weakest evidence the Eval had (Evals 16 to 18).

// aResult is the check a run that never reached a result fails: Setup
// talked through every owner turn and handed nothing over. It is held to the
// bar like any graded check, so a lone miss is a quality miss only once
// eval/gates.go names it one; until then it holds in every run.
const aResult = "a result within the owner's messages"

// noResult is a run that reached no result in its owner turns.
type noResult struct{ turns int }

func (e noResult) Error() string { return fmt.Sprintf("no result after %d owner messages", e.turns) }

// tally is what each run of a -repeat graded, and how it ended.
type tally struct {
	runs [][]eval.CheckResult
}

type tallyKey struct{}

// tallyFrom is the tally a run reports its graded checks to, or nil when it
// runs alone.
func tallyFrom(ctx context.Context) *tally {
	t, _ := ctx.Value(tallyKey{}).(*tally)
	return t
}

// record adds a run's graded checks to the tally it runs under, if any.
func record(ctx context.Context, checks []eval.CheckResult) {
	if t := tallyFrom(ctx); t != nil && len(t.runs) > 0 {
		last := &t.runs[len(t.runs)-1]
		*last = append(*last, checks...)
	}
}

// runTranscript is the transcript a run of a -repeat writes: the name as given for
// the first, then -2, -3 before its extension.
func runTranscript(name string, n int) string {
	if name == "" || n == 1 {
		return name
	}
	if i := strings.LastIndex(name, "."); i > 0 && !strings.Contains(name[i:], "/") {
		return name[:i] + "-" + strconv.Itoa(n) + name[i:]
	}
	return name + "-" + strconv.Itoa(n)
}

// withoutRepeat is args with -repeat removed and -transcript set to name.
func withoutRepeat(args []string, transcript string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		flagName := strings.TrimLeft(a, "-")
		name, _, inline := strings.Cut(flagName, "=")
		if strings.HasPrefix(a, "-") && (name == "repeat" || name == "transcript") {
			if !inline && i+1 < len(args) {
				i++ // its value
			}
			continue
		}
		out = append(out, a)
	}
	if transcript != "" {
		out = append(out, "-transcript", transcript)
	}
	return out
}

// repeatRuns runs the case n times through once, then holds the tally to the
// bar. A run that failed for anything but its graded checks or reaching no
// result fails the repeat: the bar decides graded checks, never a run that
// broke.
func repeatRuns(ctx context.Context, n int, args []string, transcript string, log io.Writer,
	once func(context.Context, []string) error) error {
	t := &tally{}
	ctx = context.WithValue(ctx, tallyKey{}, t)
	var broke []string
	for i := 1; i <= n; i++ {
		fmt.Fprintf(log, "run %d of %d\n", i, n)
		t.runs = append(t.runs, nil)
		err := once(ctx, withoutRepeat(args, runTranscript(transcript, i)))
		if err == nil {
			continue
		}
		var none noResult
		if errors.As(err, &none) {
			record(ctx, []eval.CheckResult{{Name: aResult, Grade: eval.Grade{Why: none.Error()}}})
		}
		if rest := apartFromGrades(err); rest != nil {
			broke = append(broke, fmt.Sprintf("run %d: %v", i, rest))
		}
	}
	failed := holdToBar(t.runs, log)
	switch {
	case len(broke) > 0:
		return fmt.Errorf("%d of %d runs broke: %s", len(broke), n, strings.Join(broke, "; "))
	case len(failed) > 0:
		return fmt.Errorf("over %d runs, below the bar: %s", n, strings.Join(failed, ", "))
	}
	return nil
}

// apartFromGrades is err without the failures the bar decides (graded setup
// gates, reaching no result), or nil when there are none.
func apartFromGrades(err error) error {
	var rest []error
	var walk func(error)
	walk = func(e error) {
		if j, ok := e.(interface{ Unwrap() []error }); ok {
			for _, x := range j.Unwrap() {
				walk(x)
			}
			return
		}
		var gates setupGatesFailed
		var none noResult
		if errors.As(e, &gates) || errors.As(e, &none) {
			return
		}
		rest = append(rest, e)
	}
	walk(err)
	return errors.Join(rest...)
}

// holdToBar writes each graded check's count over the runs and returns the
// ones below the bar: a safety gate that missed in any run that graded it, a
// quality gate that missed in most. Skipped and advisory checks aren't held.
func holdToBar(runs [][]eval.CheckResult, log io.Writer) []string {
	type count struct{ passed, graded int }
	counts := map[string]*count{}
	var order []string
	for _, checks := range runs {
		for _, c := range checks {
			if c.Skipped || c.Advisory {
				continue
			}
			k := counts[c.Name]
			if k == nil {
				k = &count{}
				counts[c.Name] = k
				order = append(order, c.Name)
			}
			k.graded++
			if c.Pass {
				k.passed++
			}
		}
	}
	var failed []string
	for _, name := range order {
		k := counts[name]
		quality := eval.QualityGate(name)
		holds := k.passed == k.graded
		if quality {
			holds = eval.HoldsOnMost(k.passed, k.graded)
		}
		mark, kind := "pass", "safety"
		if quality {
			kind = "quality"
		}
		if !holds {
			mark, failed = "FAIL", append(failed, name)
		} else if k.passed < k.graded {
			mark = "warn"
		}
		fmt.Fprintf(log, "  %s  %s: %d of %d runs (%s)\n", mark, name, k.passed, k.graded, kind)
	}
	return failed
}
