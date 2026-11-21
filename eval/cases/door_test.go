package cases_test

import (
	"slices"
	"testing"

	"github.com/kiff/agents/eval"
	"github.com/kiff/agents/eval/cases"
)

// A door's answer, request_outcome, is the door's own tool: answering the
// request is no write outside KIFF (Eval 44829253552 failed every door run
// that answered right on it). A call to anything else outside the gateway
// still fails the check.
func TestADoorsAnswerIsNoWriteOutsideKIFF(t *testing.T) {
	judge := &eval.Judge{Model: verdict(true)}
	answered := call{tool: "request_outcome", input: `{"outcome":"accepted"}`}
	for _, s := range []cases.Scenario{cases.DoorAccept, cases.DoorEvidence} {
		res := grade(t, s, nil, runOf("Done.", answered), judge)
		if slices.Contains(failed(res), "writes only through KIFF") {
			t.Errorf("%s: request_outcome failed \"writes only through KIFF\"", s.Name)
		}
		res = grade(t, s, nil, runOf("Done.", answered, call{tool: "send_email", server: "mail", input: `{}`}), judge)
		if !slices.Contains(failed(res), "writes only through KIFF") {
			t.Errorf("%s: a call outside KIFF passed \"writes only through KIFF\"", s.Name)
		}
	}
}
