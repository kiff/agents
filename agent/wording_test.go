package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/show"
)

var readOnly = []show.Wording{{Patterns: []string{`read-only`, `(?:ne )?modifiera rien`}, Fix: "Say what the taf's job does instead."}}

// streamed is every TextDelta a run sent, in order.
func streamed(events *[]agent.Event) func(agent.Event) {
	return func(e agent.Event) {
		if e.Kind == agent.TextDelta {
			*events = append(*events, e)
		}
	}
}

// A message that breaks a wording rule is never streamed or said: it goes back
// to the agent with the rule's fix, and the message it writes instead is what
// the owner sees, whole, once checked (Eval 44005248914: Sheets EN/FR said
// "never changes a cell" and "ne modifiera rien" in plain messages).
func TestAMessageThatBreaksAWordingRuleIsNeverShown(t *testing.T) {
	m := &script{replies: []model.Reply{say("It's a read-only connection."), say("It reads the rows the taf needs.")}}
	store, r := start(t, session.KindChat)
	a := newAgent(m)
	a.Wording = readOnly
	var deltas []agent.Event
	out, _, err := agent.Run(context.Background(), a, r, nil, "Connect my sheet.", streamed(&deltas))
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop != agent.Answered || out.Text != "It reads the rows the taf needs." || len(m.reqs) != 2 {
		t.Fatalf("stop %s, text %q, %d turns", out.Stop, out.Text, len(m.reqs))
	}
	if len(deltas) != 1 || deltas[0].Text != "It reads the rows the taf needs." {
		t.Fatalf("streamed %+v, want the checked message alone, whole", deltas)
	}
	last := m.reqs[1].Messages[len(m.reqs[1].Messages)-1]
	if last.Role != model.User || !strings.HasPrefix(last.Blocks[0].Text, agent.RewordNote) || !strings.Contains(last.Blocks[0].Text, `"read-only"`) || !strings.Contains(last.Blocks[0].Text, "Say what the taf's job does instead.") {
		t.Fatalf("the agent was told %+v", last)
	}
	if err := r.End(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, events, err := store.Read(context.Background(), "t1", r.Session().ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == session.AgentMessage && strings.Contains(string(e.Body), "read-only") {
			t.Fatalf("the held message was recorded as said: %s", e.Body)
		}
	}
	if !slicesContains(kinds(events), session.OwnRefused) {
		t.Fatalf("the held message isn't on the record as refused: %v", kinds(events))
	}
}

// Held back twice, the third is said as written: a run never stalls on a model
// that can't word it otherwise.
func TestTheThirdMatchIsSaidAsWritten(t *testing.T) {
	m := &script{replies: []model.Reply{say("read-only, 1"), say("read-only, 2"), say("read-only, 3")}}
	_, r := start(t, session.KindChat)
	a := newAgent(m)
	a.Wording = readOnly
	var deltas []agent.Event
	out, _, err := agent.Run(context.Background(), a, r, nil, "Connect my sheet.", streamed(&deltas))
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop != agent.Answered || out.Text != "read-only, 3" || len(m.reqs) != agent.Reworded+1 {
		t.Fatalf("stop %s, text %q, %d turns", out.Stop, out.Text, len(m.reqs))
	}
	if len(deltas) != 1 || deltas[0].Text != "read-only, 3" {
		t.Fatalf("streamed %+v, want only the third", deltas)
	}
}

// A held message beside tool calls: the calls still run, and the fix rides on
// their results, so the next turn words it again.
func TestAHeldMessageBesideCallsGoesBackWithTheirResults(t *testing.T) {
	lookup := &fakeTool{name: "lookup", access: tool.Read, result: tool.Result{Text: "{}"}}
	both := use("c1", "lookup", `{}`)
	both.Blocks = append([]model.Block{{Kind: model.Text, Text: "Il ne modifiera rien."}}, both.Blocks...)
	m := &script{replies: []model.Reply{both, say("C'est fait.")}}
	_, r := start(t, session.KindChat)
	a := newAgent(m, lookup)
	a.Wording = readOnly
	var deltas []agent.Event
	out, _, err := agent.Run(context.Background(), a, r, nil, "Vas-y.", streamed(&deltas))
	if err != nil {
		t.Fatal(err)
	}
	if lookup.calls != 1 || out.Text != "C'est fait." {
		t.Fatalf("calls %d, text %q", lookup.calls, out.Text)
	}
	results := m.reqs[1].Messages[len(m.reqs[1].Messages)-1]
	if results.Blocks[0].Kind != model.ToolResult || !strings.HasPrefix(results.Blocks[len(results.Blocks)-1].Text, agent.RewordNote) {
		t.Fatalf("the second turn was sent %+v", results)
	}
	if len(deltas) != 1 || deltas[0].Text != "C'est fait." {
		t.Fatalf("streamed %+v", deltas)
	}
}

// Without rules, nothing changes: text streams as the model writes it, and
// nothing more is sent once it's said.
func TestWithoutRulesTextStreamsAsBefore(t *testing.T) {
	m := &script{replies: []model.Reply{say("It's a read-only connection.")}}
	_, r := start(t, session.KindChat)
	var deltas []agent.Event
	out, _, err := agent.Run(context.Background(), newAgent(m), r, nil, "Connect my sheet.", streamed(&deltas))
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "It's a read-only connection." || len(deltas) != 1 {
		t.Fatalf("text %q, streamed %+v", out.Text, deltas)
	}
}

func slicesContains(ks []session.EventKind, k session.EventKind) bool {
	for _, x := range ks {
		if x == k {
			return true
		}
	}
	return false
}
