package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kiff/agents/eval"
)

// The Eval runs in suites (eval/suites.json): a dispatch names some, and each
// suite's steps run only when it's asked for, or all are. kiff/tafs' Upgrade
// gate takes the latest completed run on main, when it succeeded, as a full
// pass, so a run of some suites says so in its name and counts only when
// every other suite's pass carries (infra/eval-carry.sh): otherwise it ends
// red whatever its checks said.
func TestTheEvalRunsInSuitesAndOnlyAFullVerdictCountsForTheUpgrade(t *testing.T) {
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
	// Its name says so, as it does an override or another repeat, which
	// eval-carry.sh leaves out of the earlier runs.
	for _, name := range []string{"format(', suites {0}', inputs.suites)", "format(', override {0}'", "format(', repeat {0}', inputs.repeat)"} {
		if !strings.Contains(strings.SplitN(s, "\n", 7)[5], name) {
			t.Errorf("the run-name doesn't hold %s", name)
		}
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
	// A run of some suites is decided last, after every other step: red
	// below the bar's repeat or when another suite has no pass to carry.
	last := order[len(order)-1]
	if last != "A run of some suites counts only on earlier passes" {
		t.Fatalf("the last step is %q", last)
	}
	body := steps[last]
	for _, want := range []string{
		"if: always() && steps.suites.outputs.list != '' && steps.suites.outputs.list != ',all,'",
		`if [ "$REPEAT" != 3 ]; then`,
		`if ! infra/eval-carry.sh "$LIST"; then`,
		"exit 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the last step doesn't hold %q:\n%s", want, body)
		}
	}
	carry, err := os.ReadFile(filepath.Join("..", "..", "infra", "eval-carry.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`contains(\", override \") or contains(\", repeat \")`, `-carry -asked "$list"`, "set -euo pipefail"} {
		if !strings.Contains(string(carry), want) {
			t.Errorf("eval-carry.sh doesn't hold %s", want)
		}
	}
}
