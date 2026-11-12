package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every workflow that makes tafs on the test account sweeps after every run, whatever the
// runs did, and a failed sweep never fails the run: the verdict is the
// steps before it.
func TestTheSweepNeverDecidesTheRun(t *testing.T) {
	for _, wf := range []string{"eval.yml", "e2e.yml", "eval-recipe.yml"} {
		b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", wf))
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if !strings.Contains(s, "E2E_MADE: e2e-made.txt") {
			t.Fatalf("%s doesn't set E2E_MADE", wf)
		}
		i := strings.Index(s, "- name: The sweep")
		if i < 0 {
			t.Fatalf("%s has no sweep step", wf)
		}
		step := s[i:]
		if j := strings.Index(step[1:], "\n      - "); j >= 0 {
			step = step[:j+1]
		}
		for _, must := range []string{"(always())", "continue-on-error: true", "go run ./cmd/e2e -sweep", "E2E_KIFF_SESSION", "E2E_PHONE_KEY"} {
			if !strings.Contains(step, must) {
				t.Fatalf("%s's sweep step lacks %q:\n%s", wf, must, step)
			}
		}
		if strings.Contains(step, "ANTHROPIC_API_KEY") {
			t.Fatalf("%s's sweep step is given the model's key; it runs no model", wf)
		}
		// It comes after the last run of cmd/e2e.
		if last := strings.LastIndex(s, "go run ./cmd/e2e $("); last > i {
			t.Fatalf("%s runs cmd/e2e after the sweep", wf)
		}
	}
}
