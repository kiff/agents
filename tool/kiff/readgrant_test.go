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

// A read grant is resolved by KIFF with the agent's own key, into who reads
// whose work and which session; neither the key nor the grant appears in an
// error.
func TestAReadGrantIsResolvedWithTheAgentsKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/me/read-grants/resolve" || r.Header.Get("Authorization") != "Bearer "+testKey {
			http.Error(w, `{"error":"no"}`, http.StatusUnauthorized)
			return
		}
		var body struct{ Grant string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Grant != "krd_ada" {
			http.Error(w, `{"error":"read_grant"}`, http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"read": "rd_1", "reader": "ada", "owner": "ana", "session": "s1", "purpose": "fix"})
	}))
	t.Cleanup(srv.Close)
	resolve := kiff.ReadGrantResolver(oneAgent{}, keys{}, srv.Client(), srv.URL)
	ctx := context.Background()
	g, err := resolve(ctx, "t1", "rae", "krd_ada")
	if err != nil || g != (run.ReadGrant{Read: "rd_1", Reader: "ada", Owner: "ana", Session: "s1", Purpose: "fix"}) {
		t.Fatalf("ada's read: %+v, %v", g, err)
	}
	_, err = resolve(ctx, "t1", "rae", "krd_forged")
	if err == nil {
		t.Fatal("a read grant KIFF refuses resolved")
	}
	if strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), "krd_forged") {
		t.Fatalf("the error shows a secret: %v", err)
	}
}
