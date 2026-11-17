package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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

// Past the limit, every match is still said as written, and each is on the
// record as a miss, so the Eval and its metrics see it: a third, and a fourth
// in the same run.
func TestEveryMatchPastTheLimitIsRecordedAsAMiss(t *testing.T) {
	lookup := &fakeTool{name: "lookup", access: tool.Read, result: tool.Result{Text: "{}"}}
	third := use("c1", "lookup", `{}`)
	third.Blocks = append([]model.Block{{Kind: model.Text, Text: "read-only, 3"}}, third.Blocks...)
	m := &script{replies: []model.Reply{say("read-only, 1"), say("read-only, 2"), third, say("read-only, 4")}}
	store, r := start(t, session.KindChat)
	a := newAgent(m, lookup)
	a.Wording = readOnly
	out, _, err := agent.Run(context.Background(), a, r, nil, "Connect my sheet.", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "read-only, 4" || len(m.reqs) != 4 {
		t.Fatalf("text %q after %d turns", out.Text, len(m.reqs))
	}
	if err := r.End(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, events, err := store.Read(context.Background(), "t1", r.Session().ID)
	if err != nil {
		t.Fatal(err)
	}
	var held, missed int
	for _, e := range events {
		if e.Kind != session.OwnRefused {
			continue
		}
		var b agent.OwnRefusedBody
		if err := json.Unmarshal(e.Body, &b); err != nil || b.Tool != agent.MessageTool {
			t.Fatalf("a refusal %s: %v", e.Body, err)
		}
		if strings.Contains(b.Error, "read-only, ") {
			t.Fatalf("the record keeps the message, not only its phrase: %s", b.Error)
		}
		if b.Said {
			missed++
		} else {
			held++
		}
	}
	if held != agent.Reworded || missed != 2 {
		t.Fatalf("held %d, recorded as said %d; want %d and 2", held, missed, agent.Reworded)
	}
}

// The hold adds nothing past the text's own writing time: a message that
// breaks no rule is shown whole as soon as its turn returns, with no other
// turn or wait before it.
func TestACleanMessageIsShownAsSoonAsItsTurnEnds(t *testing.T) {
	m := &timedScript{script: script{replies: []model.Reply{say("It reads the rows the taf needs.")}}}
	_, r := start(t, session.KindChat)
	a := newAgent(m)
	a.Wording = readOnly
	var deltas []agent.Event
	var at time.Time
	_, _, err := agent.Run(context.Background(), a, r, nil, "Connect my sheet.", func(e agent.Event) {
		if e.Kind == agent.TextDelta {
			deltas = append(deltas, e)
			at = time.Now()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.reqs) != 1 || len(deltas) != 1 || deltas[0].Text != "It reads the rows the taf needs." {
		t.Fatalf("%d turns, streamed %+v", len(m.reqs), deltas)
	}
	if wait := at.Sub(m.returned); wait < 0 || wait > 50*time.Millisecond {
		t.Fatalf("shown %v after its turn returned", wait)
	}
}

// A closing turn has no turn left to reword in: its words are checked, said as
// written, and a match is on the record as a miss.
func TestAClosingTurnsClaimIsRecordedAsAMiss(t *testing.T) {
	lookup := &fakeTool{name: "lookup", access: tool.Read, result: tool.Result{Text: "{}"}}
	m := &script{replies: []model.Reply{use("c1", "lookup", `{}`), say("It's read-only, and set up.")}}
	store, r := start(t, session.KindChat)
	a := newAgent(m, lookup)
	a.Wording, a.CloseInText, a.MaxTurns = readOnly, true, 1
	var deltas []agent.Event
	out, _, err := agent.Run(context.Background(), a, r, nil, "Set it up.", streamed(&deltas))
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop != agent.TurnCap || out.Text != "It's read-only, and set up." || len(deltas) != 1 {
		t.Fatalf("stop %s, text %q, streamed %+v", out.Stop, out.Text, deltas)
	}
	if err := r.End(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, events, err := store.Read(context.Background(), "t1", r.Session().ID)
	if err != nil {
		t.Fatal(err)
	}
	missed := false
	for _, e := range events {
		var b agent.OwnRefusedBody
		if e.Kind == session.OwnRefused && json.Unmarshal(e.Body, &b) == nil && b.Said {
			missed = true
		}
	}
	if !missed {
		t.Fatalf("the closing turn's claim isn't on the record: %v", kinds(events))
	}
}

// A progress note is shown as it comes and never kept: one that breaks a rule
// is dropped, the others still stream.
func TestAProgressNoteThatBreaksARuleIsDropped(t *testing.T) {
	r1 := say("It reads the rows.")
	r1.Blocks = append([]model.Block{{Kind: model.Thinking, Text: "Connecting it read-only."}, {Kind: model.Thinking, Text: "Reading the sheet."}}, r1.Blocks...)
	m := &script{replies: []model.Reply{r1}}
	_, r := start(t, session.KindChat)
	a := newAgent(m)
	a.Wording, a.Thinking = readOnly, model.ThinkingUpdates
	var updates []string
	_, _, err := agent.Run(context.Background(), a, r, nil, "Connect my sheet.", func(e agent.Event) {
		if e.Kind == agent.Update {
			updates = append(updates, e.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0] != "Reading the sheet." {
		t.Fatalf("updates %q", updates)
	}
}

// timedScript is a script that notes when its last turn returned.
type timedScript struct {
	script
	returned time.Time
}

func (s *timedScript) Turn(ctx context.Context, req model.Request, onText func(string)) (model.Reply, error) {
	r, err := s.script.Turn(ctx, req, onText)
	s.returned = time.Now()
	return r, err
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
