package agent_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/tool"
)

// sideBySide is one turn calling each of calls, side by side.
func sideBySide(calls ...model.Block) model.Reply {
	return model.Reply{Blocks: calls, Stop: model.CallTools, Usage: model.Usage{InputTokens: 1000, OutputTokens: 100}}
}

func toolUse(id, name string) model.Block {
	return model.Block{Kind: model.ToolUse, CallID: id, Tool: name, Input: json.RawMessage(`{}`)}
}

// A call that acts goes in a turn of its own: sent beside the look-up meant to
// check it, it is refused back unmade while the look-up is made, and made alone
// on the next turn (kiff/tafs, the 859 device test: a refund asked for one
// second after the look-up, before its result was read).
func TestACallThatActsGoesInATurnOfItsOwn(t *testing.T) {
	lookup := &fakeTool{name: "get_charge", access: tool.Read, gateway: true, result: tool.Result{Text: `{"amount":12000}`}}
	refund := &fakeTool{name: "create_refund", access: tool.Write, gateway: true, result: tool.Result{Text: `{"status":"succeeded"}`}}
	notes := &fakeTool{name: "memory", access: tool.Own, result: tool.Result{Text: "kept"}}
	m := &script{replies: []model.Reply{
		sideBySide(toolUse("c1", "get_charge"), toolUse("c2", "create_refund"), toolUse("c3", "memory")),
		use("c4", "create_refund", `{}`),
		say("Refunded."),
	}}
	_, rec := start(t, "chat")
	if _, _, err := agent.Run(context.Background(), newAgent(m, lookup, refund, notes), rec, nil, "Refund T-5005.", nil); err != nil {
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
}
