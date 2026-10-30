package kiff_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/kiff"
)

// opKIFF is a fake gateway that keeps each call by its kiff_operation_id, as
// KIFF does: a new operation is still "deciding" the first time when slow is
// set, and sent once after; the same operation again gets its outcome; the
// same operation with other arguments is refused, nothing sent. sent counts
// what reached the vendor.
type opKIFF struct {
	slow bool
	mu   sync.Mutex
	args map[string]string // operation → its arguments
	done map[string]bool   // operation → sent
	sent int
}

func (k *opKIFF) serve(t *testing.T) *httptest.Server {
	t.Helper()
	k.args, k.done = map[string]string{}, map[string]bool{}
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
		meta := func(state, retry string) map[string]any {
			return map[string]any{kiff.MetaKey: map[string]any{"state": state, "retry": retry}}
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "close_dispute", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			var p struct {
				Arguments map[string]json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			var op string
			_ = json.Unmarshal(p.Arguments[kiff.OperationArg], &op)
			delete(p.Arguments, kiff.OperationArg)
			args, _ := json.Marshal(p.Arguments)
			k.mu.Lock()
			was, known := k.args[op]
			switch {
			case known && was != string(args):
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "Refused: kiff_operation_id " + op + " was already used for this tool with different arguments. Use a new kiff_operation_id."}},
					"isError": true, "_meta": meta("refused", "new_call")}
			case !known && k.slow:
				k.args[op] = string(args)
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "KIFF is still deciding this call."}},
					"isError": true, "_meta": meta("deciding", "same_call")}
			default:
				k.args[op] = string(args)
				if !k.done[op] {
					k.done[op] = true
					k.sent++
				}
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "closed"}}, "_meta": meta("forwarded", "none")}
			}
			k.mu.Unlock()
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// call makes the model's call callID in run, with input, through the gateway.
func (k *opKIFF) call(t *testing.T, srv *httptest.Server, runID, callID, input string) tool.Result {
	t.Helper()
	tools, err := kiff.New(srv.URL, testKey, srv.Client()).Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx := tool.WithCall(run.WithRef(context.Background(), run.Ref{Session: "s", Run: runID}), callID)
	res, err := tools[0].Call(ctx, json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// The model retries an action with its own id after KIFF said it was still
// deciding, in another call: the same operation, sent once.
func TestAModelsRetryWithItsIDIsSentOnce(t *testing.T) {
	k := &opKIFF{slow: true}
	srv := k.serve(t)
	in := `{"dispute":"T-2005","kiff_operation_id":"chb-T2005-accept"}`
	if res := k.call(t, srv, "run-1", "toolu_1", in); res.State != "deciding" {
		t.Fatalf("first try: %+v", res)
	}
	if res := k.call(t, srv, "run-1", "toolu_2", in); res.State != "forwarded" || k.sent != 1 {
		t.Fatalf("the retry: %+v, sent %d", res, k.sent)
	}
	if res := k.call(t, srv, "run-1", "toolu_3", in); res.State != "forwarded" || k.sent != 1 {
		t.Fatalf("again: %+v, sent %d", res, k.sent)
	}
}

// Two runs that use the same model id make two operations: each is sent, and
// neither is refused as a reused id.
func TestTwoRunsWithTheSameModelIDAreTwoOperations(t *testing.T) {
	k := &opKIFF{}
	srv := k.serve(t)
	in := `{"dispute":"T-2005","kiff_operation_id":"chb-T2005-accept"}`
	for _, runID := range []string{"run-1", "run-2"} {
		if res := k.call(t, srv, runID, "toolu_1", in); res.State != "forwarded" || res.IsError {
			t.Fatalf("%s: %+v", runID, res)
		}
	}
	if k.sent != 2 || len(k.args) != 2 {
		t.Fatalf("sent %d, operations %d; want 2 and 2", k.sent, len(k.args))
	}
}

// The same model id with other arguments in one run is refused, nothing sent,
// and the model's new call with a new id is made once: the skill's path.
func TestTheSameModelIDWithOtherArgumentsIsRefused(t *testing.T) {
	k := &opKIFF{}
	srv := k.serve(t)
	if res := k.call(t, srv, "run-1", "toolu_1", `{"dispute":"T-2005","amount":100,"kiff_operation_id":"chb-1"}`); res.State != "forwarded" {
		t.Fatalf("first: %+v", res)
	}
	res := k.call(t, srv, "run-1", "toolu_2", `{"dispute":"T-2005","amount":150,"kiff_operation_id":"chb-1"}`)
	if res.State != "refused" || !res.IsError || k.sent != 1 {
		t.Fatalf("other arguments: %+v, sent %d", res, k.sent)
	}
	if res := k.call(t, srv, "run-1", "toolu_3", `{"dispute":"T-2005","amount":150,"kiff_operation_id":"chb-2"}`); res.State != "forwarded" || k.sent != 2 {
		t.Fatalf("a new id: %+v, sent %d", res, k.sent)
	}
}
