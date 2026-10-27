package agent_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool"
)

// sideBySide is one turn calling each of calls, side by side.
func sideBySide(calls ...model.Block) model.Reply {
	return model.Reply{Blocks: calls, Stop: model.CallTools, Usage: model.Usage{InputTokens: 1000, OutputTokens: 100}}
}

func toolUse(id, name string) model.Block {
	return model.Block{Kind: model.ToolUse, CallID: id, Tool: name, Input: json.RawMessage(`{}`)}
}

// A call that acts never goes beside a read: sent beside the look-up meant to
// check it, it is refused back unmade while the look-up is made, and made
// on the next turn (kiff/tafs, the 859 device test: a refund asked for one
// second after the look-up, before its result was read).
func TestACallThatActsNeverGoesBesideARead(t *testing.T) {
	lookup := &fakeTool{name: "get_charge", access: tool.Read, gateway: true, result: tool.Result{Text: `{"amount":12000}`}}
	refund := &fakeTool{name: "create_refund", access: tool.Write, gateway: true, result: tool.Result{Text: `{"status":"succeeded"}`}}
	notes := &fakeTool{name: "memory", access: tool.Own, result: tool.Result{Text: "kept"}}
	m := &script{replies: []model.Reply{
		sideBySide(toolUse("c1", "get_charge"), toolUse("c2", "create_refund"), toolUse("c3", "memory")),
		use("c4", "create_refund", `{}`),
		say("Refunded."),
	}}
	store, rec := start(t, session.KindRun) // a run keeps its tool results; a chat keeps only the timing
	var streamed []agent.Event
	emit := func(e agent.Event) {
		if e.Kind == agent.CallResult {
			streamed = append(streamed, e)
		}
	}
	if _, _, err := agent.Run(context.Background(), newAgent(m, lookup, refund, notes), rec, nil, "Refund T-5005.", emit); err != nil {
		t.Fatal(err)
	}
	if lookup.calls != 1 || notes.calls != 1 || refund.calls != 1 {
		t.Fatalf("calls: look-up %d, notes %d, refund %d", lookup.calls, notes.calls, refund.calls)
	}
	// What the second turn read of the first: the look-up's result, and the refund refused unmade.
	results := m.reqs[1].Messages[len(m.reqs[1].Messages)-1].Blocks
	byCall := map[string]model.Block{}
	for _, b := range results {
		byCall[b.CallID] = b
	}
	if r := byCall["c1"]; r.IsError || r.Result != `{"amount":12000}` {
		t.Fatalf("look-up %+v", r)
	}
	if r := byCall["c2"]; !r.IsError || r.Result != agent.OneAtATime {
		t.Fatalf("refund beside the look-up %+v", r)
	}
	if r := byCall["c3"]; r.IsError {
		t.Fatalf("its own notes aren't an act: %+v", r)
	}

	// Recorded as not made, not as failed, in its result and the next turn's timing,
	// so the app shows it as not made rather than as a failed refund.
	if err := rec.Flush(context.Background()); err != nil { // Record queues; the writer stores
		t.Fatal(err)
	}
	_, events, err := store.ReadRun(context.Background(), "t1", rec.Session().ID, "r1")
	if err != nil {
		t.Fatal(err)
	}
	var resultNotMade, timingNotMade bool
	for _, e := range events {
		switch e.Kind {
		case session.ToolResult:
			var b struct {
				CallID  string `json:"call_id"`
				NotMade bool   `json:"not_made"`
			}
			if json.Unmarshal(e.Body, &b) == nil && b.NotMade {
				resultNotMade = b.CallID == "c2"
			}
		case session.Usage:
			var u session.UsageBody
			if json.Unmarshal(e.Body, &u) == nil && u.Timing != nil {
				for _, w := range u.Timing.Waited {
					if w.NotMade {
						timingNotMade = w.Tool == "create_refund"
					}
				}
			}
		}
	}
	// And on the stream, as the app's work block reads it while the run goes on.
	for _, e := range streamed {
		if e.NotMade != (e.CallID == "c2") {
			t.Fatalf("streamed result %s not made %v", e.CallID, e.NotMade)
		}
	}
	if len(streamed) != 4 {
		t.Fatalf("streamed %d results", len(streamed))
	}
	if !resultNotMade || !timingNotMade {
		t.Fatalf("not made: in its result %v, in the next turn's timing %v", resultNotMade, timingNotMade)
	}
}

// Acts side by side go together: what each acts on was read in an earlier
// turn, so answering three checked orders stays one turn.
func TestActsSideBySideGoTogether(t *testing.T) {
	refund := &fakeTool{name: "create_refund", access: tool.Write, gateway: true, result: tool.Result{Text: `{"status":"succeeded"}`}}
	notes := &fakeTool{name: "memory", access: tool.Own, result: tool.Result{Text: "kept"}}
	m := &script{replies: []model.Reply{
		sideBySide(toolUse("c1", "create_refund"), toolUse("c2", "create_refund"), toolUse("c3", "memory")),
		say("Refunded both."),
	}}
	_, rec := start(t, session.KindRun)
	if _, _, err := agent.Run(context.Background(), newAgent(m, refund, notes), rec, nil, "Refund T-5001 and T-5002.", nil); err != nil {
		t.Fatal(err)
	}
	if refund.calls != 2 || notes.calls != 1 {
		t.Fatalf("calls: refunds %d, notes %d", refund.calls, notes.calls)
	}
	for _, b := range m.reqs[1].Messages[len(m.reqs[1].Messages)-1].Blocks {
		if b.IsError {
			t.Fatalf("an act beside another act was refused: %+v", b)
		}
	}
}
