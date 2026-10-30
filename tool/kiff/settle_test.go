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

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/kiff"
)

// settling is a fake gateway for an unknown outcome: the call op-u was sent
// and its response lost (invocation tc-u); reconciling it answers code; the
// call sent again as kiff.Again("op-u") goes through. It keeps the
// operation IDs of the calls it took and the reconciles it was asked.
type settling struct {
	code       int
	mu         sync.Mutex
	ops        []string
	reconciled []string
}

func (s *settling) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/reconcile") {
			s.mu.Lock()
			s.reconciled = append(s.reconciled, r.URL.Path)
			s.mu.Unlock()
			w.WriteHeader(s.code)
			msg := map[int]string{200: "final no-effect outcome reconciled; action remains spent",
				409: "original executor status lookup unavailable; budget remains charged",
				503: "status lookup did not settle the outcome; budget remains charged"}[s.code]
			_ = json.NewEncoder(w).Encode(map[string]any{"message": msg, "operation_reopened": false})
			return
		}
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
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "stripe_refund", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			var p struct {
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			op, _ := p.Arguments[kiff.OperationArg].(string)
			s.mu.Lock()
			s.ops = append(s.ops, op)
			s.mu.Unlock()
			if op == "op-u" {
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "Sent to the tool, but its response was lost."}},
					"isError": true, "_meta": map[string]any{kiff.MetaKey: map[string]any{"state": "unknown", "invocation": "tc-u", "retry": "none"}}}
			} else {
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "refunded re_2"}},
					"_meta": map[string]any{kiff.MetaKey: map[string]any{"state": "forwarded", "invocation": "tc-again", "retry": "none"}}}
			}
		}
		resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// An unknown outcome is settled once by KIFF's read-only status lookup: when
// it proves the first try had no effect, the call goes once more, under an
// ID fixed from the first; when it can't (409, 503), the outcome stays
// unknown with what KIFF said, and nothing is sent. Any other outcome isn't
// reconciled at all.
func TestSettleAsksKIFFOnceAboutAnUnknownOutcome(t *testing.T) {
	for _, c := range []struct {
		code      int
		wantState string
		wantOps   []string
		wantText  string
	}{
		{200, "forwarded", []string{"op-u", kiff.Again("op-u")}, "proved the first try had no effect"},
		{409, "unknown", []string{"op-u"}, "KIFF checked: original executor status lookup unavailable"},
		{503, "unknown", []string{"op-u"}, "KIFF checked: status lookup did not settle the outcome"},
	} {
		s := &settling{code: c.code}
		srv := s.serve(t)
		tools, err := kiff.New(srv.URL+"/mcp", testKey, srv.Client()).Tools(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		a := agent.Agent{Tools: tools}
		q := run.Requirement{CallID: "c1", Tool: "stripe_refund", Input: json.RawMessage(`{"charge":"ch_1"}`), Operation: "op-u", Kind: run.KindResume}
		got, err := kiff.Collect(context.Background(), a, []run.Requirement{q})
		if err != nil {
			t.Fatal(err)
		}
		res, err := kiff.Settle(context.Background(), a, q, got[0].Result)
		if err != nil {
			t.Fatalf("%d: %v", c.code, err)
		}
		if res.State != c.wantState || !strings.Contains(res.Text, c.wantText) {
			t.Fatalf("%d: settled to %+v", c.code, res)
		}
		if strings.Join(s.ops, " ") != strings.Join(c.wantOps, " ") {
			t.Fatalf("%d: calls %v, want %v", c.code, s.ops, c.wantOps)
		}
		if len(s.reconciled) != 1 || s.reconciled[0] != "/v1/operations/tc-u/reconcile" {
			t.Fatalf("%d: reconciled %v", c.code, s.reconciled)
		}
	}

	// An outcome that isn't unknown is left as it is: KIFF isn't asked.
	s := &settling{code: 200}
	srv := s.serve(t)
	tools, err := kiff.New(srv.URL+"/mcp", testKey, srv.Client()).Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := tool.Result{Text: "refunded re_1", State: "forwarded", Invocation: "tc-1"}
	if res, err := kiff.Settle(context.Background(), agent.Agent{Tools: tools}, run.Requirement{CallID: "c1", Tool: "stripe_refund", Operation: "op-1"}, done); err != nil || res.Text != done.Text || res.State != done.State || len(s.reconciled) != 0 {
		t.Fatalf("a known outcome: %+v, %v, reconciled %v", res, err, s.reconciled)
	}
}

// A link's gateway reconciles under its own path.
func TestALinksCallIsReconciledUnderItsPath(t *testing.T) {
	s := &settling{code: 409}
	srv := s.serve(t)
	tools, err := kiff.New(srv.URL+"/a/lnk_1/mcp", testKey, srv.Client()).Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a := agent.Agent{Tools: tools}
	q := run.Requirement{CallID: "c1", Tool: "stripe_refund", Operation: "op-u"}
	if _, err := kiff.Settle(context.Background(), a, q, tool.Result{State: "unknown", Invocation: "tc-u"}); err != nil {
		t.Fatal(err)
	}
	if len(s.reconciled) != 1 || s.reconciled[0] != "/a/lnk_1/operations/tc-u/reconcile" {
		t.Fatalf("reconciled %v", s.reconciled)
	}
}
