package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A setup-only Eval (cases: setup) runs Coach's three setup chats and skips
// the rest, and never counts for kiff/tafs' Upgrade gate: the gate takes the
// latest completed run on main, when it succeeded, as a full pass, so such a
// run says so in its name and ends red whatever its checks said.
func TestASetupOnlyEvalNeverCountsForTheUpgrade(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "eval.yml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	step := func(name string) string {
		t.Helper()
		i := strings.Index(s, "      - name: "+name)
		if i < 0 {
			t.Fatalf("eval.yml has no step %q", name)
		}
		rest := s[i:]
		if j := strings.Index(rest[1:], "\n      - "); j >= 0 {
			rest = rest[:j+1]
		}
		return rest
	}
	if !strings.Contains(s, "      cases:\n") || !strings.Contains(s, `options: ["all", "setup"]`) || !strings.Contains(s, `default: "all"`) {
		t.Fatal("eval.yml has no cases input of all (the default) or setup")
	}
	if !strings.Contains(s, "run-name: ${{ inputs.cases == 'setup' && 'Eval, setup only' ||") {
		t.Fatal("a setup-only run doesn't say so in its name")
	}
	// The other cases are skipped; the setup chats always run.
	for _, name := range []string{"Chargebacks, two seeded runs", "Hand-offs between tafs, a sender and a receiver", "Tickets, a recipe's taf on the fake desk"} {
		if !strings.Contains(step(name), "&& inputs.cases != 'setup'") {
			t.Errorf("%q runs in a setup-only Eval", name)
		}
	}
	for _, name := range []string{"Setup with the taf, in English, graded", "Setup on the second vendor, a desk by its key", "Setup on billing, invoices that stay drafts"} {
		if strings.Contains(step(name), "inputs.cases") {
			t.Errorf("%q depends on cases: it runs in both", name)
		}
	}
	// It ends red, after every other step, whatever they said.
	last := step("A setup-only run doesn't count for the Upgrade gate")
	if !strings.Contains(last, "if: always() && inputs.cases == 'setup'") || !strings.Contains(last, "exit 1") {
		t.Fatalf("the setup-only end doesn't fail the run:\n%s", last)
	}
	if !strings.HasSuffix(strings.TrimRight(s, "\n"), strings.TrimRight(last, "\n")) {
		t.Fatal("the setup-only end isn't the last step")
	}
}
