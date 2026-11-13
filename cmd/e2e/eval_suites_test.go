package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kiff/agents/eval"
)

// The Eval runs in suites (eval/suites.json): a dispatch names some, and each
// suite's steps run only when it's asked for, or all are. A run of some
// suites never counts for kiff/tafs' Upgrade gate, which takes the latest
// completed run on main, when it succeeded, as a full pass: it says so in its
// name and ends red whatever its checks said.
func TestTheEvalRunsInSuitesAndOnlyAllCountsForTheUpgrade(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "eval.yml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	steps := map[string]string{}
	var order []string
	for _, part := range strings.Split(s, "\n      - ")[1:] {
		name, _, _ := strings.Cut(part, "\n")
		name = strings.TrimPrefix(name, "name: ")
		steps[name] = part
		order = append(order, name)
	}
	suites, err := eval.LoadSuites()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, "      suites:\n") || !strings.Contains(s, `default: "all"`) {
		t.Fatal("eval.yml has no suites input defaulting to all")
	}
	if !strings.Contains(s, "run-name: ${{ inputs.suites != '' && inputs.suites != 'all' && format('Eval, suites {0}', inputs.suites) ||") {
		t.Fatal("a run of some suites doesn't say so in its name")
	}
	if !strings.Contains(steps["The suites to run"], "go run ./cmd/evalsuites -check -asked") {
		t.Fatal("the asked suites aren't checked against eval/suites.json")
	}
	// Each suite's steps run when it's asked for, or all are.
	inSuite := map[string]string{}
	for _, name := range suites.Names() {
		for _, st := range suites.Suites[name].Steps {
			body, ok := steps[st]
			if !ok {
				t.Fatalf("suite %s names a step eval.yml doesn't have: %q", name, st)
			}
			if other, dup := inSuite[st]; dup {
				t.Fatalf("%q is in suites %s and %s", st, other, name)
			}
			inSuite[st] = name
			want := "(contains(steps.suites.outputs.list, ',all,') || contains(steps.suites.outputs.list, '," + name + ",'))"
			if !strings.Contains(body, want) {
				t.Errorf("%q doesn't run only with suite %s or all", st, name)
			}
		}
	}
	// Every step that runs a case is in a suite: none runs whatever was asked.
	for _, name := range order {
		body := steps[name]
		if (strings.Contains(body, "go run ./cmd/eval -run") || strings.Contains(body, "go run ./cmd/e2e $(")) && inSuite[name] == "" {
			t.Errorf("%q runs a case but is in no suite", name)
		}
	}
	// A run of some suites ends red, after every other step.
	last := order[len(order)-1]
	if last != "A run of some suites doesn't count for the Upgrade gate" {
		t.Fatalf("the last step is %q", last)
	}
	if body := steps[last]; !strings.Contains(body, "if: always() && steps.suites.outputs.list != '' && steps.suites.outputs.list != ',all,'") || !strings.Contains(body, "exit 1") {
		t.Fatalf("a run of some suites doesn't end red:\n%s", body)
	}
}
