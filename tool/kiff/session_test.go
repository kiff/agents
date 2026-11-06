package kiff_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/kiff"
)

// A gateway tool that declares SessionArg is told which chat each call comes
// from: kiff/agents sets it to the run's session, over anything the model put
// there, and the model never sees it in the schema. A tool that doesn't
// declare it never gets it (kiff/tafs#544: a Coach drive is its chat's alone).
func TestAToolThatAsksForTheSessionGetsTheRunsOwn(t *testing.T) {
	var mu sync.Mutex
	sent := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &msg)
		if msg.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s")
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
		case "tools/list":
			result = map[string]any{"tools": []any{
				map[string]any{"name": "app_look", "inputSchema": map[string]any{"type": "object",
					"properties": map[string]any{"tree": map[string]any{"type": "boolean"}, kiff.SessionArg: map[string]any{"type": "string"}},
					"required":   []any{kiff.SessionArg}}, "annotations": map[string]any{"readOnlyHint": true}},
				map[string]any{"name": "stripe_refund", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"charge": map[string]any{"type": "string"}}}},
			}}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			mu.Lock()
			sent[p.Name] = p.Arguments
			mu.Unlock()
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
	}))
	defer srv.Close()
	tools, err := kiff.New(srv.URL, testKey, srv.Client()).Tools(context.Background())
	if err != nil || len(tools) != 2 {
		t.Fatalf("tools = %v, %v", tools, err)
	}
	look, refund := tools[0], tools[1]

	// The model never sees it, nor is told it's required.
	if spec := string(look.Spec().InputSchema); strings.Contains(spec, kiff.SessionArg) || !strings.Contains(spec, `"tree"`) {
		t.Fatalf("the model's schema: %s", spec)
	}

	// In a run, the run's session, over the model's word for it.
	inRun := tool.WithCall(run.WithRef(context.Background(), run.Ref{Session: "s_coach_chat", Run: "run-1"}), "call-1")
	if _, err := look.Call(inRun, json.RawMessage(`{"tree":true,"`+kiff.SessionArg+`":"s_someone_else"}`)); err != nil {
		t.Fatal(err)
	}
	if got := sent["app_look"][kiff.SessionArg]; got != "s_coach_chat" || sent["app_look"]["tree"] != true {
		t.Fatalf("sent %v", sent["app_look"])
	}

	// Outside a run there is no chat: whatever the model put is taken out.
	if _, err := look.Call(context.Background(), json.RawMessage(`{"`+kiff.SessionArg+`":"s_made_up"}`)); err != nil {
		t.Fatal(err)
	}
	if _, has := sent["app_look"][kiff.SessionArg]; has {
		t.Fatalf("outside a run, sent %v", sent["app_look"])
	}

	// A tool that didn't ask never gets it, not even the model's own (qa-kiff on #342):
	// a tool whose stored schema predates the argument must never be told a chat by the model.
	if _, err := refund.Call(inRun, json.RawMessage(`{"charge":"ch_1","`+kiff.SessionArg+`":"s_chat_a"}`)); err != nil {
		t.Fatal(err)
	}
	if _, has := sent["stripe_refund"][kiff.SessionArg]; has || sent["stripe_refund"]["charge"] != "ch_1" {
		t.Fatalf("a tool that didn't ask got it: %v", sent["stripe_refund"])
	}
}
