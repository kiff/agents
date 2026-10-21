package anthropic_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

// serve answers every request with events, and keeps the last request body.
func serve(t *testing.T, events []string) (*anthropic.Model, *map[string]any) {
	t.Helper()
	sent := new(map[string]any)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*sent = map[string]any{} // this request's, not merged into the last
		_ = json.Unmarshal(body, sent)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range events {
			var head struct{ Type string }
			_ = json.Unmarshal([]byte(ev), &head)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", head.Type, ev)
		}
	}))
	t.Cleanup(srv.Close)
	return anthropic.New(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0)), sent
}

// thinkingStream is a turn that thinks, then answers.
var thinkingStream = []string{
	`{"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":"test-model","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"The refund is under the Card."}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-abc"}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"enc-xyz"}}`,
	`{"type":"content_block_stop","index":1}`,
	`{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"Done."}}`,
	`{"type":"content_block_stop","index":2}`,
	`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}`,
	`{"type":"message_stop"}`,
}

// Thinking comes back as opaque blocks, in order, and goes back to the API
// exactly as it came, in place: dropping it breaks the next turn (#68).
func TestThinkingIsKeptAndSentBackInPlace(t *testing.T) {
	m, sent := serve(t, thinkingStream)
	reply, err := m.Turn(context.Background(), model.Request{Model: "test-model", MaxTokens: 512,
		Messages: []model.Message{{Role: model.User, Blocks: []model.Block{{Kind: model.Text, Text: "Refund ch_1."}}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []model.Block{
		{Kind: model.Thinking, Text: "The refund is under the Card.", Signature: "sig-abc"},
		{Kind: model.RedactedThinking, Data: "enc-xyz"},
		{Kind: model.Text, Text: "Done."},
	}
	if fmt.Sprint(reply.Blocks) != fmt.Sprint(want) {
		t.Fatalf("blocks %+v\nwant %+v", reply.Blocks, want)
	}

	// The next turn replays them before the tool call, unchanged.
	_, err = m.Turn(context.Background(), model.Request{Model: "test-model", MaxTokens: 512, Messages: []model.Message{
		{Role: model.User, Blocks: []model.Block{{Kind: model.Text, Text: "Refund ch_1."}}},
		{Role: model.Assistant, Blocks: append(want[:2:2], model.Block{Kind: model.ToolUse, CallID: "toolu_1", Tool: "refund", Input: json.RawMessage(`{}`)})},
		{Role: model.User, Blocks: []model.Block{{Kind: model.ToolResult, CallID: "toolu_1", Result: "ok"}}},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal((*sent)["messages"].([]any)[1].(map[string]any)["content"])
	if got := string(b); got != `[{"signature":"sig-abc","thinking":"The refund is under the Card.","type":"thinking"},{"data":"enc-xyz","type":"redacted_thinking"},{"id":"toolu_1","input":{},"name":"refund","type":"tool_use"}]` {
		t.Fatalf("sent back %s", got)
	}
}

// An agent's effort goes as output_config.effort; none sends nothing, and
// the API's default applies.
func TestEffortIsSent(t *testing.T) {
	m, sent := serve(t, thinkingStream)
	msgs := []model.Message{{Role: model.User, Blocks: []model.Block{{Kind: model.Text, Text: "Hi."}}}}
	if _, err := m.Turn(context.Background(), model.Request{Model: "test-model", MaxTokens: 512, Messages: msgs, Effort: "high"}, nil); err != nil {
		t.Fatal(err)
	}
	if oc, _ := (*sent)["output_config"].(map[string]any); oc["effort"] != "high" {
		t.Fatalf("output_config %v", (*sent)["output_config"])
	}
	if _, err := m.Turn(context.Background(), model.Request{Model: "test-model", MaxTokens: 512, Messages: msgs}, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := (*sent)["output_config"]; ok {
		t.Fatalf("no effort sent output_config %v", (*sent)["output_config"])
	}
}

// A refused turn is its own stop, with the category the API gives (#68).
func TestARefusalIsItsOwnStop(t *testing.T) {
	m, _ := serve(t, []string{
		`{"type":"message_start","message":{"id":"msg_3","type":"message","role":"assistant","model":"test-model","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber","explanation":"declined"}},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	})
	reply, err := m.Turn(context.Background(), model.Request{Model: "test-model", MaxTokens: 512,
		Messages: []model.Message{{Role: model.User, Blocks: []model.Block{{Kind: model.Text, Text: "x"}}}}}, nil)
	if err != nil || reply.Stop != model.Refused || reply.Refusal != "cyber" {
		t.Fatalf("stop %q, refusal %q, %v", reply.Stop, reply.Refusal, err)
	}
}

// cached lists where body has cache_control: "system", "tool <name>", or
// "message <i> block <j>".
func cached(body map[string]any) []string {
	var at []string
	marked := func(v any) bool { m, _ := v.(map[string]any); return m["cache_control"] != nil }
	for _, b := range asList(body["system"]) {
		if marked(b) {
			at = append(at, "system")
		}
	}
	for _, tl := range asList(body["tools"]) {
		if marked(tl) {
			at = append(at, "tool "+tl.(map[string]any)["name"].(string))
		}
	}
	for i, m := range asList(body["messages"]) {
		for j, b := range asList(m.(map[string]any)["content"]) {
			if marked(b) {
				at = append(at, fmt.Sprintf("message %d block %d", i, j))
			}
		}
	}
	return at
}

func asList(v any) []any { l, _ := v.([]any); return l }

func TestCacheMarksThePrefixAndTheLastMessage(t *testing.T) {
	tools := []model.ToolSpec{
		{Name: "a", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "b", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	msgs := []model.Message{
		{Role: model.User, Blocks: []model.Block{{Kind: model.Text, Text: "go"}}},
		{Role: model.Assistant, Blocks: []model.Block{{Kind: model.Thinking, Text: "t", Signature: "s"}, {Kind: model.ToolUse, CallID: "c1", Tool: "a", Input: json.RawMessage(`{}`)}}},
		{Role: model.User, Blocks: []model.Block{{Kind: model.ToolResult, CallID: "c1", Result: "ok"}}},
	}
	cases := map[string]struct {
		req  model.Request
		want string
	}{
		"system and the last result": {model.Request{Model: "m", MaxTokens: 1, System: "be brief", Tools: tools, Messages: msgs, Cache: true},
			"system,message 2 block 0"},
		"no system: the last tool": {model.Request{Model: "m", MaxTokens: 1, Tools: tools, Messages: msgs, Cache: true},
			"tool b,message 2 block 0"},
		"a last message ending in thinking": {model.Request{Model: "m", MaxTokens: 1, System: "s", Cache: true, Messages: []model.Message{
			{Role: model.User, Blocks: []model.Block{{Kind: model.Text, Text: "go"}}},
			{Role: model.Assistant, Blocks: []model.Block{{Kind: model.Text, Text: "x"}, {Kind: model.Thinking, Text: "t", Signature: "s"}}},
		}}, "system,message 1 block 0"},
		"no cache": {model.Request{Model: "m", MaxTokens: 1, System: "s", Tools: tools, Messages: msgs}, ""},
	}
	for name, c := range cases {
		if got := strings.Join(cached(sent(t, c.req)), ","); got != c.want {
			t.Errorf("%s: cache_control at %q, want %q", name, got, c.want)
		}
	}
}

// An agent whose price counts an hour's writes keeps the shared prefix (its
// tools and system prompt) an hour, and the conversation five minutes: an
// owner's next message often comes after a pause (kiff/tafs#138, item 58).
func TestCacheLongKeepsThePrefixAnHour(t *testing.T) {
	ttl := func(v any) any {
		m, _ := v.(map[string]any)
		c, _ := m["cache_control"].(map[string]any)
		return c["ttl"]
	}
	msgs := []model.Message{{Role: model.User, Blocks: []model.Block{{Kind: model.Text, Text: "go"}}}}
	body := sent(t, model.Request{Model: "m", MaxTokens: 1, System: "s", Messages: msgs, Cache: true, CacheLong: true})
	sys := asList(body["system"])
	last := asList(asList(body["messages"])[0].(map[string]any)["content"])[0]
	if ttl(sys[len(sys)-1]) != "1h" || ttl(last) != nil {
		t.Fatalf("system ttl %v, last message ttl %v", ttl(sys[len(sys)-1]), ttl(last))
	}
	short := sent(t, model.Request{Model: "m", MaxTokens: 1, System: "s", Messages: msgs, Cache: true})
	if sys := asList(short["system"]); ttl(sys[len(sys)-1]) != nil {
		t.Fatalf("five minutes: system ttl %v", ttl(sys[len(sys)-1]))
	}
}

// The hour's cache writes are counted apart, priced at their own rate.
func TestAnHoursCacheWritesAreCountedApart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{
			`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1,"cache_creation_input_tokens":1300,"cache_creation":{"ephemeral_1h_input_tokens":1000,"ephemeral_5m_input_tokens":300}}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			`{"type":"message_stop"}`,
		} {
			var head struct{ Type string }
			_ = json.Unmarshal([]byte(ev), &head)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", head.Type, ev)
		}
	}))
	defer srv.Close()
	m := anthropic.New(option.WithBaseURL(srv.URL), option.WithAPIKey("k"), option.WithMaxRetries(0))
	reply, err := m.Turn(context.Background(), model.Request{Model: "m", MaxTokens: 8, System: "s", Cache: true, CacheLong: true,
		Messages: []model.Message{{Role: model.User, Blocks: []model.Block{{Kind: model.Text, Text: "go"}}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Usage.CacheWrite != 300 || reply.Usage.CacheWrite1h != 1000 {
		t.Fatalf("usage %+v", reply.Usage)
	}
	p := model.Price{Input: 4_000_000, Output: 20_000_000, CacheRead: 400_000, CacheWrite: 5_000_000, CacheWrite1h: 8_000_000}
	// 5*4 + 2*20 + 300*5 + 1000*8 = 9560 millionths of a dollar per million: 10 rounded up.
	if got := p.Cost(reply.Usage); got != (5*4_000_000+2*20_000_000+300*5_000_000+1000*8_000_000+999_999)/1_000_000 {
		t.Fatalf("cost %d", got)
	}
	if !p.CachesAnHour() || (model.Price{Input: 1, Output: 1, CacheRead: 1, CacheWrite: 1}).CachesAnHour() {
		t.Fatal("CachesAnHour")
	}
}

// The context window comes from the Models API, asked once per model; a
// failure, asked without retries, is remembered too.
func TestTheWindowIsAskedOnce(t *testing.T) {
	asked := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked[r.URL.Path]++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models/gone" {
			http.Error(w, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`, http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"type":"model","id":"m","display_name":"M","created_at":"2026-01-01T00:00:00Z","max_input_tokens":1000000,"max_tokens":128000}`)
	}))
	defer srv.Close()
	m := anthropic.New(option.WithBaseURL(srv.URL), option.WithAPIKey("k"))
	for range 2 {
		if w, err := m.Window(context.Background(), "m"); err != nil || w != 1_000_000 {
			t.Fatalf("window %d, %v", w, err)
		}
		if _, err := m.Window(context.Background(), "gone"); err == nil {
			t.Fatal("a failed ask gave a window")
		}
	}
	if asked["/v1/models/m"] != 1 || asked["/v1/models/gone"] != 1 {
		t.Fatalf("asked %v", asked)
	}
}

// Window waits only so long for the Models API, and remembers that; a turn
// cancelled while it asks is not remembered, so the next one asks again.
func TestTheWindowNeverStallsATurn(t *testing.T) {
	defer anthropic.SetWindowWait(50 * time.Millisecond)()
	var mu sync.Mutex
	asked := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked[r.URL.Path]++
		mu.Unlock()
		if r.URL.Path == "/v1/models/slow" {
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"type":"model","id":"m","display_name":"M","created_at":"2026-01-01T00:00:00Z","max_input_tokens":1000000,"max_tokens":128000}`)
	}))
	defer srv.Close()
	m := anthropic.New(option.WithBaseURL(srv.URL), option.WithAPIKey("k"))
	for range 2 {
		start := time.Now()
		if _, err := m.Window(context.Background(), "slow"); err == nil || time.Since(start) > time.Second {
			t.Fatalf("a hung Models API: %v after %s", err, time.Since(start))
		}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Window(cancelled, "m"); err == nil {
		t.Fatal("a cancelled turn got a window")
	}
	if w, err := m.Window(context.Background(), "m"); err != nil || w != 1_000_000 {
		t.Fatalf("after a cancelled ask: %d, %v", w, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if asked["/v1/models/slow"] != 1 {
		t.Fatalf("asked the slow model %d times", asked["/v1/models/slow"])
	}
}
