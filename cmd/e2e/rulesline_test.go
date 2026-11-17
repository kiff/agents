package main

import (
	"strings"
	"testing"

	"github.com/kiff/agents/tool/show"
)

// A run names the rules it was held to by digest: the same rules give the same
// line, and one more pattern gives another.
func TestARunNamesTheRulesItWasHeldTo(t *testing.T) {
	rules := []show.Wording{{Patterns: []string{"read-only"}, Fix: "Say what the job does."}}
	line := rulesLine(rules)
	if line != rulesLine([]show.Wording{{Patterns: []string{"read-only"}, Fix: "Say what the job does."}}) {
		t.Fatal("the same rules gave two lines")
	}
	if !strings.HasPrefix(line, "rules: Coach's wording 1 rules, ") || !strings.Contains(line, planNoHarmlessClaim) || !strings.Contains(line, planWiderOnlyAfter) {
		t.Fatalf("line %q", line)
	}
	more := []show.Wording{{Patterns: []string{"read-only", "reading only"}, Fix: "Say what the job does."}}
	if rulesLine(more) == line {
		t.Fatal("another pattern gave the same line")
	}
}
