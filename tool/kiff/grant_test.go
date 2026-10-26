package kiff_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/tool/kiff"
)

// A run grant is resolved by KIFF with the agent's own key, and neither
// the key nor the grant appears in an error.
func TestAGrantIsResolvedWithTheAgentsKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/me/run-grants/resolve" || r.Header.Get("Authorization") != "Bearer "+testKey {
			http.Error(w, `{"error":"no"}`, http.StatusUnauthorized)
			return
		}
		var body struct{ Grant string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Grant != "krg_ana" {
			http.Error(w, `{"error":"bad_grant"}`, http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"subject": "ana", "agent": "rae"})
	}))
	t.Cleanup(srv.Close)
	resolve := kiff.GrantResolver(oneAgent{}, keys{}, srv.Client(), srv.URL)
	ctx := context.Background()
	if who, err := resolve(ctx, "t1", "rae", "krg_ana"); err != nil || who != "ana" {
		t.Fatalf("ana's grant: %q, %v", who, err)
	}
	_, err := resolve(ctx, "t1", "rae", "krg_forged")
	if err == nil {
		t.Fatal("a grant KIFF refuses resolved")
	}
	if strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), "krg_forged") {
		t.Fatalf("the error shows a secret: %v", err)
	}
}

// A run's grant goes on each of its gateway calls, so KIFF records the
// call, and any hold it opens, as the person's; a call with none carries
// none.
func TestARunsGrantGoesOnItsCalls(t *testing.T) {
	var seen []string
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
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "list_charges", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			g := r.Header.Get(kiff.RunGrantHeader)
			if g == "" {
				g = "-"
			}
			seen = append(seen, g)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	tools, err := kiff.New(srv.URL, testKey, srv.Client()).Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tools[0].Call(run.WithGrant(context.Background(), "krg_ana"), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := tools[0].Call(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "krg_ana" || seen[1] != "-" {
		t.Fatalf("grant headers %v, want [krg_ana -]", seen)
	}
}
