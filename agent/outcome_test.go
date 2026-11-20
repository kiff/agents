package agent

import (
	"testing"

	"github.com/kiff/agents/tool"
)

// A hold's outcome says who answered it when it was the owner: approved by
// the owner, or refused by the owner. A judge read "held by KIFF, refused,
// not sent" as KIFF refusing, and failed a taf that told its owner "You
// refused the €9 refund" (Eval 43670959911). Any other word keeps its own, so
// an expired hold is never put down to the owner.
func TestAHoldSaysWhoAnsweredIt(t *testing.T) {
	refused := ToolResultBody{Result: tool.Result{Text: "not sent", IsError: true, State: tool.StateRefused}}
	sent := ToolResultBody{Result: tool.Result{Text: "re_1"}}
	for _, c := range []struct {
		name   string
		r      ToolResultBody
		answer string
		want   string
	}{
		{"the owner refused", refused, "refused", OutcomeDeclined},
		{"the owner rejected", refused, "rejected", OutcomeDeclined},
		{"the hold expired", refused, "expired", "held by KIFF, expired, not sent"},
		{"KIFF refused, no hold", refused, "", OutcomeRefused},
		{"the owner approved", sent, "approved", OutcomeApproved},
		{"the Card changed", sent, "changed", OutcomeChanged},
	} {
		if got := outcome(c.r, c.answer); got != c.want {
			t.Errorf("%s: outcome = %q, want %q", c.name, got, c.want)
		}
	}
	if TallyOf(OutcomeDeclined) != "refused" || TallyOf("held by KIFF, expired, not sent") != "refused" {
		t.Error("a refused or expired hold no longer counts as refused")
	}
}
