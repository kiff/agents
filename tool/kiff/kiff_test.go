package kiff_test

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

	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/kiff"
)

const testKey = "kiff_test_agent_key"

// gateway is a fake KIFF gateway: it checks the bearer key and the session
// header, lists two tools, and holds every refund. With sse set, it answers
// requests as an event stream.
func gateway(t *testing.T, sse bool) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var msg struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &msg); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		mu.Lock()
		methods = append(methods, msg.Method)
		mu.Unlock()
		if msg.Method != "initialize" && r.Header.Get("Mcp-Session-Id") != "sess-1" {
			http.Error(w, "no session", http.StatusBadRequest)
			return
		}
		if msg.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
		case "tools/list":
			result = map[string]any{"tools": []any{
				map[string]any{"name": "stripe_get_charge", "inputSchema": map[string]any{"type": "object"}, "annotations": map[string]any{"readOnlyHint": true}},
				map[string]any{"name": "stripe_refund", "description": "Refund a charge", "inputSchema": map[string]any{"type": "object"}},
			}}
		case "tools/call":
			var p struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			if p.Name == "stripe_refund" {
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "Held: the owner answers on their phone."}},
					"isError": true, "_meta": map[string]any{"dev.kiff/call": map[string]any{"state": "held", "sent": "no", "retry": "same_call"}}}
			} else {
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"id":"ch_1"}`}}}
			}
		default:
			result = nil
		}
		resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
		if sse {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", resp)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &methods
}

func TestListsAndCallsToolsThroughTheGateway(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			srv, methods := gateway(t, sse)
			tools, err := kiff.New(srv.URL, testKey, srv.Client()).Tools(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(tools) != 2 || tools[0].Access() != tool.Read || tools[1].Access() != tool.Write || !tools[1].Gateway() {
				t.Fatalf("tools = %v", tools)
			}
			if err := tool.Check(tools); err != nil {
				t.Fatal(err)
			}
			got, err := tools[0].Call(context.Background(), json.RawMessage(`{"charge":"ch_1"}`))
			if err != nil || got.Text != `{"id":"ch_1"}` || got.IsError || got.Held() {
				t.Fatalf("read = %+v %v", got, err)
			}
			held, err := tools[1].Call(context.Background(), nil)
			if err != nil || !held.Held() || !held.IsError {
				t.Fatalf("refund = %+v %v", held, err)
			}
			want := "initialize notifications/initialized tools/list tools/call tools/call"
			if strings.Join(*methods, " ") != want {
				t.Fatalf("methods = %v", *methods)
			}
		})
	}
}

// A refused key never shows up in the error.
func TestTheKeyStaysOutOfErrors(t *testing.T) {
	srv, _ := gateway(t, false)
	_, err := kiff.New(srv.URL, "kiff_wrong_key", srv.Client()).Tools(context.Background())
	if err == nil || strings.Contains(err.Error(), "kiff_wrong_key") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
}

// A JSON-RPC error that echoes the key, on initialize or on a call, never
// carries it on: not into the error, and not into what the model reads.
func TestTheKeyStaysOutOfGatewayErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		if msg.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var resp any
		switch msg.Method {
		case "initialize":
			resp = map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": map[string]any{}}
		case "tools/list":
			resp = map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": map[string]any{"tools": []any{map[string]any{"name": "stripe_refund"}}}}
		default:
			resp = map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "error": map[string]any{"code": -32000, "message": "bad token " + r.Header.Get("Authorization")}}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	tools, err := kiff.New(srv.URL, testKey, srv.Client()).Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = tools[0].Call(context.Background(), nil)
	if err == nil || strings.Contains(err.Error(), testKey) || !strings.Contains(err.Error(), "[key]") {
		t.Fatalf("err = %v", err)
	}

	initErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32001, "message": "no session for " + r.Header.Get("Authorization")}})
	}))
	defer initErr.Close()
	_, err = kiff.New(initErr.URL, testKey, initErr.Client()).Tools(context.Background())
	if err == nil || strings.Contains(err.Error(), testKey) {
		t.Fatalf("initialize err = %v", err)
	}
}
