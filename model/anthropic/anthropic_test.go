package anthropic_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/model/anthropic"
)

// stream is a recorded Messages API stream: some text, then a tool call.
var stream = []string{
	`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"test-model","content":[],"stop_reason":null,"usage":{"input_tokens":120,"output_tokens":1,"cache_read_input_tokens":40}}}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Looking "}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"it up."}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"stripe_get_charge","input":{}}}`,
	`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"charge\":\"ch_1\"}"}}`,
	`{"type":"content_block_stop","index":1}`,
	`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":30}}`,
	`{"type":"message_stop"}`,
}

func TestATurnStreamsTextAndReturnsToolCalls(t *testing.T) {
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &sent)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range stream {
			var head struct{ Type string }
			_ = json.Unmarshal([]byte(ev), &head)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", head.Type, ev)
		}
	}))
	defer srv.Close()

	m := anthropic.New(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
	var text strings.Builder
	reply, err := m.Turn(context.Background(), model.Request{
		Model: "test-model", System: "Be brief.", MaxTokens: 512,
		Messages: []model.Message{
			{Role: model.User, Blocks: []model.Block{{Kind: model.Text, Text: "Check ch_1."}}},
			{Role: model.Assistant, Blocks: []model.Block{{Kind: model.ToolUse, CallID: "toolu_0", Tool: "stripe_get_charge", Input: json.RawMessage(`{}`)}}},
			{Role: model.User, Blocks: []model.Block{{Kind: model.ToolResult, CallID: "toolu_0", Result: "not found", IsError: true}}},
		},
		Tools: []model.ToolSpec{{Name: "stripe_get_charge", Description: "Read a charge",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"charge":{"type":"string"}},"required":["charge"]}`)}},
	}, func(s string) { text.WriteString(s) })
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != "Looking it up." {
		t.Fatalf("streamed %q", text.String())
	}
	want := []model.Block{{Kind: model.Text, Text: "Looking it up."},
		{Kind: model.ToolUse, CallID: "toolu_1", Tool: "stripe_get_charge", Input: json.RawMessage(`{"charge":"ch_1"}`)}}
	if len(reply.Blocks) != 2 || reply.Blocks[0].Text != want[0].Text || reply.Blocks[1].CallID != "toolu_1" ||
		string(reply.Blocks[1].Input) != string(want[1].Input) {
		t.Fatalf("blocks = %+v", reply.Blocks)
	}
	if reply.Stop != model.CallTools || reply.Usage.InputTokens != 120 || reply.Usage.OutputTokens != 30 || reply.Usage.CacheRead != 40 {
		t.Fatalf("stop %s usage %+v", reply.Stop, reply.Usage)
	}
	// What was sent: the model id, the system prompt, the three messages and the tool.
	if sent["model"] != "test-model" || len(sent["messages"].([]any)) != 3 || len(sent["tools"].([]any)) != 1 {
		t.Fatalf("sent = %v", sent)
	}
	tl := sent["tools"].([]any)[0].(map[string]any)
	if tl["name"] != "stripe_get_charge" || tl["input_schema"].(map[string]any)["required"].([]any)[0] != "charge" {
		t.Fatalf("tool sent = %v", tl)
	}
}

// A tool's whole input schema reaches the API: $defs a property refers to,
// and root constraints such as additionalProperties.
func TestTheWholeToolSchemaIsSent(t *testing.T) {
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &sent)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{stream[0], `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`, `{"type":"message_stop"}`} {
			var head struct{ Type string }
			_ = json.Unmarshal([]byte(ev), &head)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", head.Type, ev)
		}
	}))
	defer srv.Close()
	schema := `{"type":"object","properties":{"amount":{"$ref":"#/$defs/amount"}},"required":["amount"],` +
		`"additionalProperties":false,"$defs":{"amount":{"type":"integer","minimum":1}}}`
	m := anthropic.New(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
	_, err := m.Turn(context.Background(), model.Request{Model: "test-model", MaxTokens: 64,
		Messages: []model.Message{{Role: model.User, Blocks: []model.Block{{Kind: model.Text, Text: "Refund 5."}}}},
		Tools:    []model.ToolSpec{{Name: "refund", InputSchema: json.RawMessage(schema)}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(sent["tools"].([]any)[0].(map[string]any)["input_schema"])
	var a, b any
	_ = json.Unmarshal(got, &a)
	_ = json.Unmarshal([]byte(schema), &b)
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatalf("sent schema %s\nwant %s", got, schema)
	}
}

// sent is the request body the adapter sends for req.
func sent(t *testing.T, req model.Request) map[string]any {
	t.Helper()
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{stream[0], `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`, `{"type":"message_stop"}`} {
			var head struct{ Type string }
			_ = json.Unmarshal([]byte(ev), &head)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", head.Type, ev)
		}
	}))
	defer srv.Close()
	m := anthropic.New(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
	if _, err := m.Turn(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	return body
}

// A turn that forbids tool use still sends the tools, with tool_choice none:
// its messages hold a tool call and its result, which a request without tools
// can't carry. A turn that doesn't forbid it sends no tool_choice.
func TestNoToolUseSendsTheToolsAndChoiceNone(t *testing.T) {
	req := model.Request{Model: "test-model", MaxTokens: 64, NoToolUse: true,
		Messages: []model.Message{
			{Role: model.User, Blocks: []model.Block{{Kind: model.Text, Text: "Refund ch_1."}}},
			{Role: model.Assistant, Blocks: []model.Block{{Kind: model.ToolUse, CallID: "c1", Tool: "refund", Input: json.RawMessage(`{"charge":"ch_1"}`)}}},
			{Role: model.User, Blocks: []model.Block{{Kind: model.ToolResult, CallID: "c1", Result: "Held for the owner.", IsError: true}}},
		},
		Tools: []model.ToolSpec{{Name: "refund", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	body := sent(t, req)
	if tools, _ := body["tools"].([]any); len(tools) != 1 {
		t.Fatalf("sent tools %v", body["tools"])
	}
	if choice, _ := body["tool_choice"].(map[string]any); choice["type"] != "none" || len(choice) != 1 {
		t.Fatalf("sent tool_choice %v", body["tool_choice"])
	}
	req.NoToolUse = false
	if body := sent(t, req); body["tool_choice"] != nil {
		t.Fatalf("a turn allowed to use tools sent tool_choice %v", body["tool_choice"])
	}
	req.NoToolUse, req.Tools, req.Messages = true, nil, req.Messages[:1]
	if body := sent(t, req); body["tool_choice"] != nil || body["tools"] != nil {
		t.Fatalf("a turn with no tools sent tools %v, tool_choice %v", body["tools"], body["tool_choice"])
	}
}
