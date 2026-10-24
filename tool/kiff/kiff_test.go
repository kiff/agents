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
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/kiff"
)

const testKey = "kiff_test_agent_key"

// calls are the arguments of each tools/call the fake gateway received, and
// slow counts the repeats of "op-slow".
var (
	calls []string
	slow  atomic.Int32
)

// gateway is a fake KIFF gateway: it checks the bearer key and the session
// header, lists two tools, and holds every refund. With sse set, it answers
// requests as an event stream.
func gateway(t *testing.T, sse bool) (*httptest.Server, *[]string) {
	t.Helper()
	calls = nil
	slow.Store(0)
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
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			var args map[string]any
			_ = json.Unmarshal(p.Arguments, &args)
			mu.Lock()
			calls = append(calls, string(p.Arguments))
			mu.Unlock()
			// A repeat with an operation ID collects the call: KIFF knows the
			// owner's answer by it.
			op, _ := args["kiff_operation_id"].(string)
			if op == "op-approved" || op == "op-refund-42" || op == "tc-77" { // the holds below, answered
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "refunded re_1"}},
					"_meta": map[string]any{"dev.kiff/call": map[string]any{"state": "forwarded", "sent": "yes", "retry": "none"}}}
			} else if op == "op-expired" {
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "The owner did not answer before the hold expired."}},
					"isError": true, "_meta": map[string]any{"dev.kiff/call": map[string]any{"state": "refused", "sent": "no", "retry": "new_call"}}}
			} else if op == "op-deciding" || op == "op-forwarding" || (op == "op-slow" && slow.Add(1) == 1) {
				// KIFF is still deciding or sending the call: no outcome yet.
				// "op-slow" is deciding once, then answered.
				state := strings.TrimPrefix(op, "op-")
				if op == "op-slow" {
					state = "deciding"
				}
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "KIFF is still on this call."}},
					"isError": true, "_meta": map[string]any{"dev.kiff/call": map[string]any{"state": state, "sent": "no", "retry": "same_call"}}}
			} else if op == "op-slow" {
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "refunded re_1"}},
					"_meta": map[string]any{"dev.kiff/call": map[string]any{"state": "forwarded", "sent": "yes", "retry": "none"}}}
			} else if op == "op-held-no-retry" {
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "Held."}},
					"isError": true, "_meta": map[string]any{"dev.kiff/call": map[string]any{"state": "held"}}}
			} else if op == "op-failed" { // approved, but KIFF couldn't send it
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "The tool could not be reached, so the call was not sent."}},
					"isError": true, "_meta": map[string]any{"dev.kiff/call": map[string]any{"state": "failed", "sent": "no", "retry": "new_call"}}}
			} else if op == "op-unknown" { // sent, and its response lost
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "Sent to the tool, but its response was lost."}},
					"isError": true, "_meta": map[string]any{"dev.kiff/call": map[string]any{"state": "unknown", "sent": "unknown", "retry": "none"}}}
			} else if op == "op-error" {
				http.Error(w, "gateway down", http.StatusBadGateway)
				return
			} else if p.Name == "stripe_refund" && args["keyless"] == true {
				// Called without kiff_operation_id: KIFF keys it by its own
				// "tc-…" invocation key, and sends no operation_id.
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "Held."}},
					"isError": true, "_meta": map[string]any{"dev.kiff/call": map[string]any{
						"state": "held", "invocation": "tc-77", "exception_id": "exc_789", "sent": "no", "retry": "same_call"}}}
			} else if o, _ := args["outcome"].(string); p.Name == "stripe_refund" && o != "" {
				// Held, and collected later as "op-<outcome>".
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "Held."}},
					"isError": true, "_meta": map[string]any{"dev.kiff/call": map[string]any{
						"state": "held", "operation_id": "op-" + o, "exception_id": "exc_" + o, "sent": "no", "retry": "same_call",
						"hold_expires_at": "2026-10-16T09:10:00Z"}}}
			} else if p.Name == "stripe_refund" && args["slow"] == true {
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "Held."}},
					"isError": true, "_meta": map[string]any{"dev.kiff/call": map[string]any{
						"state": "held", "operation_id": "op-slow", "exception_id": "exc_456", "sent": "no", "retry": "same_call"}}}
			} else if p.Name == "stripe_refund" && args["odd"] == true {
				// KIFF's hold fields in a shape this client doesn't expect.
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "Held."}},
					"isError": true, "_meta": map[string]any{"dev.kiff/call": map[string]any{
						"state": "held", "operation_id": "op-refund-42", "exception_id": "exc_123", "hold_expires_at": 1760605800}}}
			} else if p.Name == "stripe_refund" && args["nested"] == true {
				// A sealed connection carries the resource deeper in the arguments.
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "refunded re_10"}},
					"_meta": map[string]any{"dev.kiff/call": map[string]any{"state": "forwarded", "invocation": "tc-90",
						"resource_argument": "charge", "resource_path": "parameters.charge", "sent": "yes", "retry": "none"}}}
			} else if p.Name == "stripe_refund" && args["forwarded"] == true {
				// Within its Card: sent, and KIFF names its key (kiff/tafs#167).
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "refunded re_9"}},
					"_meta": map[string]any{"dev.kiff/call": map[string]any{"state": "forwarded", "invocation": "tc-88", "resource_argument": "payment", "sent": "yes", "retry": "none"}}}
			} else if p.Name == "stripe_refund" {
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "Held: the owner answers on their phone."}},
					"isError": true, "_meta": map[string]any{
						// A tool's own namespace, passed through by the gateway, with
						// a field of the same name and another type.
						"example.com/audit": map[string]any{"hold_expires_at": 1720000000},
						"dev.kiff/call": map[string]any{
							"state": "held", "invocation": "tc-01", "operation_id": "op-refund-42", "sent": "no",
							"exception_id": "exc_123", "review_url": "https://app.kiff.dev/review/exc_123",
							"hold_expires_at": "2026-10-16T09:10:00Z", "retry": "same_call", "retry_after_s": 30}}}
			} else {
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"id":"ch_1"}`}},
					"_meta": map[string]any{"example.com/audit": map[string]any{"hold_expires_at": 1720000000}}}
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
			if err != nil || got.Text != `{"id":"ch_1"}` || got.IsError || got.Held() || got.Hold != nil || got.Invocation != "" {
				t.Fatalf("read = %+v %v", got, err)
			}
			held, err := tools[1].Call(context.Background(), nil)
			if err != nil || !held.Held() || !held.IsError {
				t.Fatalf("refund = %+v %v", held, err)
			}
			// The held call's handle in KIFF, from the gateway's _meta.
			wantHold := tool.Hold{Operation: "op-refund-42", Exception: "exc_123", Expires: time.Date(2026, 10, 16, 9, 10, 0, 0, time.UTC)}
			if held.Hold == nil || *held.Hold != wantHold {
				t.Fatalf("hold = %+v, want %+v", held.Hold, wantHold)
			}
			// KIFF's key for the call, which its activity and receipt file it under.
			if held.Invocation != "tc-01" {
				t.Fatalf("held invocation = %q", held.Invocation)
			}
			want := "initialize notifications/initialized tools/list tools/call tools/call"
			if strings.Join(*methods, " ") != want {
				t.Fatalf("methods = %v", *methods)
			}
		})
	}
}

// A call KIFF sent within its Card keeps KIFF's key for it and, by the
// argument KIFF names, what it acted on, so the Record can name the payment
// (kiff/tafs#167).
func TestAForwardedCallKeepsKIFFsKey(t *testing.T) {
	srv, _ := gateway(t, false)
	tools, err := kiff.New(srv.URL, testKey, srv.Client()).Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, err := tools[1].Call(context.Background(), json.RawMessage(`{"forwarded":true,"payment":"pi_9","amount":15000}`))
	if err != nil || got.IsError || got.Held() || got.Text != "refunded re_9" || got.Invocation != "tc-88" || got.Resource != "pi_9" {
		t.Fatalf("forwarded = %+v %v", got, err)
	}
	// The key is recorded with the result; what it's on is not (the run keeps
	// it as its call_ref, past the result).
	raw, _ := json.Marshal(got)
	if !strings.Contains(string(raw), `"invocation":"tc-88"`) || strings.Contains(string(raw), "pi_9") {
		t.Fatalf("recorded result = %s", raw)
	}
	// Where KIFF says it is, nested; a number there, or a path that isn't, is nothing.
	for input, want := range map[string]string{
		`{"nested":true,"parameters":{"charge":"ch_7","amount":100}}`: "ch_7",
		`{"nested":true,"parameters":{"charge":42}}`:                  "",
		`{"nested":true,"charge":"ch_top"}`:                           "",
		`{"nested":true,"parameters":"ch_7"}`:                         "",
	} {
		got, err := tools[1].Call(context.Background(), json.RawMessage(input))
		if err != nil || got.Invocation != "tc-90" || got.Resource != want {
			t.Fatalf("%s: resource %q, want %q (%v)", input, got.Resource, want, err)
		}
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

// Only state says a call is held: a hold field KIFF sends in another shape is
// left empty, and the call is still held, so the run still pauses (and, with
// no expiry, fails closed) instead of going on as if the call had failed.
func TestAHoldStaysHeldWhenAFieldDoesntParse(t *testing.T) {
	srv, _ := gateway(t, false)
	tools, err := kiff.New(srv.URL, testKey, srv.Client()).Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	held, err := tools[1].Call(context.Background(), json.RawMessage(`{"odd":true}`))
	if err != nil || !held.Held() || held.Hold == nil {
		t.Fatalf("got %+v, %v; want it held", held, err)
	}
	if *held.Hold != (tool.Hold{Operation: "op-refund-42", Exception: "exc_123"}) {
		t.Fatalf("hold %+v, want the IDs and no expiry", *held.Hold)
	}
}

// Setup's reads (kiff/tafs#138): a tool the gateway doesn't list to the
// agent is still called by its name, with the agent's key, and KIFF decides
// it on the agent's Card. Nothing is listed first.
func TestCallsAToolByNameWithoutListing(t *testing.T) {
	srv, methods := gateway(t, false)
	got, err := kiff.New(srv.URL, testKey, srv.Client()).Call(context.Background(), "stripe_get_charge", json.RawMessage(`{"charge":"ch_1"}`))
	if err != nil || got.Text != `{"id":"ch_1"}` || got.IsError {
		t.Fatalf("read = %+v %v", got, err)
	}
	if want := "initialize notifications/initialized tools/call"; strings.Join(*methods, " ") != want {
		t.Fatalf("methods = %v", *methods)
	}
}

// A read that the gateway answers 502 (a deploy, a cold start) is tried once
// more: opening the session and listing tools. An action never is: a
// tools/call answered 502 may have been sent, so it fails as it came. A
// second 502 on a read fails too.
func TestA502IsRetriedOnceOnReadsOnly(t *testing.T) {
	kiff.ReadRetryWait = time.Millisecond
	var mu sync.Mutex
	seen := map[string]int{}
	failFirst := map[string]bool{"initialize": true, "tools/list": true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		mu.Lock()
		seen[msg.Method]++
		n := seen[msg.Method]
		mu.Unlock()
		if (failFirst[msg.Method] && n == 1) || msg.Method == "tools/call" {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		if msg.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any = map[string]any{"capabilities": map[string]any{}}
		if msg.Method == "tools/list" {
			result = map[string]any{"tools": []any{map[string]any{"name": "stripe_refund", "inputSchema": map[string]any{"type": "object"}}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
	}))
	defer srv.Close()
	c := kiff.New(srv.URL, testKey, srv.Client())
	tools, err := c.Tools(context.Background())
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools: %v %v", tools, err)
	}
	if _, err := tools[0].Call(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("call: %v", err)
	}
	if seen["initialize"] != 2 || seen["tools/list"] != 2 || seen["tools/call"] != 1 {
		t.Fatalf("sent: %v", seen)
	}
	// Two 502s in a row: the read fails.
	always := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer always.Close()
	if _, err := kiff.New(always.URL, testKey, always.Client()).Tools(context.Background()); err == nil {
		t.Fatal("a gateway that keeps answering 502 listed tools")
	}
}
