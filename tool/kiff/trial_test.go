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

// trialGateway is a gateway that declares trials when trials is set, and
// answers a call made with KIFF-Trial with a decision, never a hold. It
// records each tools/call's KIFF-Trial header ("-" when absent).
func trialGateway(t *testing.T, trials bool) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
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
			caps := map[string]any{}
			if trials {
				caps["experimental"] = map[string]any{kiff.TrialCapability: true}
			}
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": caps}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "create_refund", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			h := r.Header.Get(kiff.TrialHeader)
			if h == "" {
				h = "-"
			}
			mu.Lock()
			seen = append(seen, h)
			mu.Unlock()
			if h == "-" { // an ordinary call over the limit: held
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "Held."}}, "isError": true,
					"_meta": map[string]any{kiff.MetaKey: map[string]any{"state": "held", "operation_id": "op-1", "exception_id": "exc_1"}}}
			} else {
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "Trial: KIFF would hold this call. Nothing was sent."}},
					"_meta": map[string]any{kiff.MetaKey: map[string]any{"state": "trial",
						"trial": map[string]any{"decision": "held", "reason": "300.00 EUR is above the 100.00 EUR limit for one call", "title": "Create refund"}}}}
			}
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// kiff/tafs#107: a trial's call names its draft to KIFF, comes back with
// KIFF's decision and never pauses the run.
func TestATrialsCallIsDecidedNotSent(t *testing.T) {
	srv, seen := trialGateway(t, true)
	tools, err := kiff.New(srv.URL, testKey, srv.Client()).Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx := run.WithTrial(context.Background(), run.Trial{Draft: "drf_2"})
	got, err := tools[0].Call(ctx, json.RawMessage(`{"charge":"ch_1","amount":30000}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Held() || got.Hold != nil || got.State != "" {
		t.Fatalf("a trial paused: %+v", got)
	}
	if got.Trial == nil || *got.Trial != (tool.TrialDecision{Decision: "held", Reason: "300.00 EUR is above the 100.00 EUR limit for one call", Title: "Create refund"}) {
		t.Fatalf("decision = %+v", got.Trial)
	}
	// Against the live Cards when the version proposes none.
	if _, err := tools[0].Call(run.WithTrial(context.Background(), run.Trial{}), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	// An ordinary call carries no trial header, and is held as before.
	held, err := tools[0].Call(context.Background(), json.RawMessage(`{}`))
	if err != nil || !held.Held() || held.Trial != nil {
		t.Fatalf("ordinary call = %+v, %v", held, err)
	}
	if want := []string{"drf_2", "live", "-"}; len(*seen) != 3 || (*seen)[0] != want[0] || (*seen)[1] != want[1] || (*seen)[2] != want[2] {
		t.Fatalf("headers %v, want %v", *seen, want)
	}
}

// A gateway that hasn't declared trials would send a trial's call for
// real: it gets nothing.
func TestATrialSendsNothingToAGatewayWithoutTrials(t *testing.T) {
	srv, seen := trialGateway(t, false)
	tools, err := kiff.New(srv.URL, testKey, srv.Client()).Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = tools[0].Call(run.WithTrial(context.Background(), run.Trial{Draft: "drf_2"}), json.RawMessage(`{}`))
	if err == nil || len(*seen) != 0 {
		t.Fatalf("err %v, calls %v: want the call refused locally and nothing sent", err, *seen)
	}
}
