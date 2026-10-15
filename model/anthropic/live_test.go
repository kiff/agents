package anthropic_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/model/anthropic"
)

// TestLiveTurn calls the real Claude API: it streams text, calls a tool, and
// reads the tool's result back. It runs only with ANTHROPIC_API_KEY and
// AGENTS_LIVE_MODEL set, never in CI, so it costs nothing unasked:
//
//	ANTHROPIC_API_KEY=… AGENTS_LIVE_MODEL=claude-haiku-5-5 go test -run Live ./model/anthropic/
func TestLiveTurn(t *testing.T) {
	modelID := os.Getenv("AGENTS_LIVE_MODEL")
	if os.Getenv("ANTHROPIC_API_KEY") == "" || modelID == "" {
		t.Skip("set ANTHROPIC_API_KEY and AGENTS_LIVE_MODEL to call the real API")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	m := anthropic.New()
	tools := []model.ToolSpec{{Name: "get_charge", Description: "Read a card charge by its id.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"charge":{"type":"string"}},"required":["charge"]}`)}}
	msgs := []model.Message{{Role: model.User, Blocks: []model.Block{{Kind: model.Text, Text: "What is the amount of charge ch_1? Use the tool."}}}}

	streamed := 0
	first, err := m.Turn(ctx, model.Request{Model: modelID, MaxTokens: 512, Messages: msgs, Tools: tools}, func(s string) { streamed += len(s) })
	if err != nil {
		t.Fatal(err)
	}
	var call *model.Block
	for i, b := range first.Blocks {
		if b.Kind == model.ToolUse {
			call = &first.Blocks[i]
		}
	}
	if first.Stop != model.CallTools || call == nil || call.Tool != "get_charge" || !strings.Contains(string(call.Input), "ch_1") {
		t.Fatalf("first turn: stop %s, blocks %+v", first.Stop, first.Blocks)
	}
	if first.Usage.InputTokens == 0 || first.Usage.OutputTokens == 0 {
		t.Fatalf("no usage: %+v", first.Usage)
	}

	msgs = append(msgs, model.Message{Role: model.Assistant, Blocks: first.Blocks},
		model.Message{Role: model.User, Blocks: []model.Block{{Kind: model.ToolResult, CallID: call.CallID, Result: `{"id":"ch_1","amount":4200,"currency":"eur"}`}}})
	second, err := m.Turn(ctx, model.Request{Model: modelID, MaxTokens: 512, Messages: msgs, Tools: tools}, func(s string) { streamed += len(s) })
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, b := range second.Blocks {
		text.WriteString(b.Text)
	}
	if second.Stop != model.EndTurn || !strings.Contains(text.String(), "42") || streamed == 0 {
		t.Fatalf("second turn: stop %s, text %q, streamed %d bytes", second.Stop, text.String(), streamed)
	}

	// The turn the loop takes when KIFF holds a call: the conversation holds
	// a tool call and its result, and the model must answer in text. The API
	// takes it with the tools offered and tool use forbidden.
	held := append(msgs[:1:1], model.Message{Role: model.Assistant, Blocks: first.Blocks},
		model.Message{Role: model.User, Blocks: []model.Block{{Kind: model.ToolResult, CallID: call.CallID, Result: "Held for the owner's answer.", IsError: true}}})
	third, err := m.Turn(ctx, model.Request{Model: modelID, MaxTokens: 256, Messages: held, Tools: tools, NoToolUse: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range third.Blocks {
		if b.Kind == model.ToolUse {
			t.Fatalf("a turn with tool use forbidden called %s", b.Tool)
		}
	}
	if third.Stop != model.EndTurn || len(third.Blocks) == 0 {
		t.Fatalf("third turn: stop %s, blocks %+v", third.Stop, third.Blocks)
	}
}
