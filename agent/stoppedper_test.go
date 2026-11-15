package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool"
)

// itemTool is an app's write that fails for the items in bad and takes the
// rest, counting what reached it by input.
type itemTool struct {
	name    string
	bad     map[string]bool
	refused bool // a refusal by KIFF on the Card, not an app's error
	mu      sync.Mutex
	reached map[string]int
}

func (s *itemTool) Spec() model.ToolSpec {
	return model.ToolSpec{Name: s.name, InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (s *itemTool) Access() tool.Access { return tool.Write }
func (s *itemTool) Gateway() bool       { return true }
func (s *itemTool) Call(_ context.Context, input json.RawMessage) (tool.Result, error) {
	var in struct {
		Dispute string `json:"dispute"`
	}
	_ = json.Unmarshal(input, &in)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reached == nil {
		s.reached = map[string]int{}
	}
	s.reached[in.Dispute]++
	switch {
	case s.refused:
		return tool.Result{Text: "refused: outside the Card", IsError: true, State: tool.StateRefused}, nil
	case s.bad[in.Dispute] || s.bad["*"]:
		return tool.Result{Text: "Stripe answered 404: no such dispute", IsError: true}, nil
	}
	return tool.Result{Text: `{"id":"` + in.Dispute + `","status":"under_review"}`, State: tool.StateForwarded}, nil
}

func dispute(id, d string) model.Reply {
	return use(id, "update_dispute", `{"dispute":"`+d+`","evidence":"…"}`)
}

// The chargebacks taf (Eval 43674391351, run 2): two evidence calls failed on
// a mistyped dispute id, and the run's rule then stopped every other
// dispute's evidence. An app's error now counts per call: the mistyped one
// isn't tried a third time, and the other disputes are still settled.
// Category: availability
// Attack: none; the regression was a run that stopped doing its job
// Refused: the third try of the mistyped call, unmade; the others are made
func TestOneItemsErrorsDontStopTheOthers(t *testing.T) {
	up := &itemTool{name: "update_dispute", bad: map[string]bool{"du_typo": true}}
	m := &script{replies: []model.Reply{
		dispute("c1", "du_typo"),
		dispute("c2", "du_typo"),
		dispute("c3", "du_1"),
		dispute("c4", "du_2"),
		dispute("c5", "du_typo"),
		say("Settled du_1 and du_2. One dispute id was wrong, so nothing went on it."),
	}}
	_, r := start(t, session.KindRun)
	if _, _, err := agent.Run(context.Background(), newAgent(m, up), r, nil, "Answer the open disputes.", nil); err != nil {
		t.Fatal(err)
	}
	if up.reached["du_1"] != 1 || up.reached["du_2"] != 1 {
		t.Fatalf("the other disputes reached the app %v, want once each", up.reached)
	}
	if up.reached["du_typo"] != 2 {
		t.Fatalf("the mistyped call reached the app %d times, want 2 (one try, one more)", up.reached["du_typo"])
	}
	if got, _ := resultOf(m.reqs, "c5"); got != agent.AfterTwoFailures {
		t.Fatalf("the third mistyped call came back %q", got)
	}
}

// The same call is the same however its input's keys are ordered or spaced:
// reordering them isn't a way to a third try.
// Category: authority
// Attack: the failed call again, its keys reordered and respaced
// Refused: the third try, unmade
func TestAReorderedCallIsTheSameCall(t *testing.T) {
	up := &itemTool{name: "update_dispute", bad: map[string]bool{"du_typo": true}}
	m := &script{replies: []model.Reply{
		use("c1", "update_dispute", `{"dispute":"du_typo","evidence":"a"}`),
		use("c2", "update_dispute", `{"evidence":"a","dispute":"du_typo"}`),
		use("c3", "update_dispute", `{ "evidence" : "a" , "dispute" : "du_typo" }`),
		say("It failed twice."),
	}}
	_, r := start(t, session.KindRun)
	if _, _, err := agent.Run(context.Background(), newAgent(m, up), r, nil, "Answer the dispute.", nil); err != nil {
		t.Fatal(err)
	}
	if up.reached["du_typo"] != 2 {
		t.Fatalf("the call reached the app %d times, want 2", up.reached["du_typo"])
	}
	if got, _ := resultOf(m.reqs, "c3"); got != agent.AfterTwoFailures {
		t.Fatalf("the reordered third try came back %q", got)
	}
}

// An app that errs on every call (its sign-in ended, say) still stops the
// run's use of the tool, after FailuresPerTool errors on any inputs.
// Category: availability
// Attack: a different item each time, every one failing
// Refused: every call past FailuresPerTool, unmade
func TestAToolThatErrsOnEveryCallStillStops(t *testing.T) {
	up := &itemTool{name: "update_dispute", bad: map[string]bool{"*": true}}
	var replies []model.Reply
	for i := 1; i <= agent.FailuresPerTool+2; i++ {
		replies = append(replies, dispute("c"+string(rune('0'+i)), "du_"+string(rune('0'+i))))
	}
	replies = append(replies, say("Stripe refused every dispute: sign in again."))
	m := &script{replies: replies}
	_, r := start(t, session.KindRun)
	if _, _, err := agent.Run(context.Background(), newAgent(m, up), r, nil, "Answer the open disputes.", nil); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, n := range up.reached {
		total += n
	}
	if total != agent.FailuresPerTool {
		t.Fatalf("%d calls reached the app, want %d", total, agent.FailuresPerTool)
	}
	for i := agent.FailuresPerTool + 1; i <= agent.FailuresPerTool+2; i++ {
		if got, _ := resultOf(m.reqs, "c"+string(rune('0'+i))); got != agent.AfterTwoFailures {
			t.Fatalf("call %d came back %q", i, got)
		}
	}
}

// A call KIFF refused on the Card is the owner's limit on the tool, whatever
// the item: twice refused, the tool isn't called again in the run, on any
// input.
// Category: authority
// Attack: a different item after each refusal
// Refused: the third call, unmade, though its item is new
func TestKIFFsRefusalStillCountsPerTool(t *testing.T) {
	up := &itemTool{name: "update_dispute", refused: true}
	m := &script{replies: []model.Reply{
		dispute("c1", "du_1"),
		dispute("c2", "du_2"),
		dispute("c3", "du_3"),
		say("KIFF refused the evidence."),
	}}
	_, r := start(t, session.KindRun)
	if _, _, err := agent.Run(context.Background(), newAgent(m, up), r, nil, "Answer the open disputes.", nil); err != nil {
		t.Fatal(err)
	}
	if up.reached["du_3"] != 0 || up.reached["du_1"]+up.reached["du_2"] != 2 {
		t.Fatalf("reached the app: %v", up.reached)
	}
	if got, _ := resultOf(m.reqs, "c3"); got != agent.AfterTwoFailures || !strings.HasPrefix(got, "Not made") {
		t.Fatalf("the third call came back %q", got)
	}
}
