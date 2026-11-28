package agent

import (
	"encoding/json"
	"testing"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/tool"
)

// A call held inside the agent (Your step, a card, a wait) backs no claim:
// nothing came of it yet. Only once the owner answered and it was done does it.
func TestAHeldCallBacksNoClaim(t *testing.T) {
	for _, state := range []string{tool.StateHeld, tool.StateAwaiting, tool.StateWaiting} {
		msgs := []model.Message{
			{Role: model.Assistant, Blocks: []model.Block{{Kind: model.ToolUse, CallID: "c1", Tool: "send_invoice", Input: json.RawMessage(`{"action":"send"}`)}}},
			{Role: model.User, Blocks: []model.Block{resultBlock(ToolResultBody{CallID: "c1", Tool: "send_invoice", Result: tool.Result{Text: "waits for the owner", State: state}})}},
		}
		if b := msgs[1].Blocks[0]; b.State != state || b.IsError {
			t.Fatalf("%s: the block is %+v", state, b)
		}
		var a Agent
		if done := a.doneIn(msgs); len(done) != 0 {
			t.Fatalf("%s: a held call backs %v", state, done)
		}
		// Answered and done, it does.
		a.resolved = map[string]string{"c1": OutcomeDone}
		msgs[1].Blocks[0].State = ""
		if done := a.doneIn(msgs); !done["send_invoice"] || !done["send_invoice:send"] {
			t.Fatalf("%s: answered and done, it backs %v", state, done)
		}
	}
	// A call that went through, with no state, backs its claim as before.
	var a Agent
	msgs := []model.Message{
		{Role: model.Assistant, Blocks: []model.Block{{Kind: model.ToolUse, CallID: "c2", Tool: "memory", Input: json.RawMessage(`{"action":"write"}`)}}},
		{Role: model.User, Blocks: []model.Block{resultBlock(ToolResultBody{CallID: "c2", Tool: "memory", Result: tool.Result{Text: "kept"}})}},
	}
	if done := a.doneIn(msgs); !done["memory:write"] {
		t.Fatalf("a write that went through: %v", done)
	}
}
