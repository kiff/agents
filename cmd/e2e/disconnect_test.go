package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kiff/agents/cmd/internal/testaccount"
)

// A grant an earlier run left connected is disconnected through planque's
// own route, signed by the test account's phone, and read back: done only
// when planque says disconnected. Another account's grant (planque: not
// found) is never sent a disconnect. The public log names only the grant's
// first characters.
func TestADisconnectIsTheTestAccountsOwnAndReadBack(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	var mu sync.Mutex
	status := map[string]string{"grt_qwBr3staleone": "connected", "grt_gone0000000": "disconnected"}
	deletes := 0
	planque := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Planque-Device-Signature") == "" {
			http.Error(w, `{"error":"unsigned"}`, http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/accounts/tnt/grants":
			var gs []map[string]string
			for id, st := range status {
				gs = append(gs, map[string]string{"id": id, "status": st})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"grants": gs})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/accounts/tnt/grants/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/accounts/tnt/grants/")
			if _, ok := status[id]; !ok {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			deletes++
			status[id] = "disconnected"
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "status": "disconnected"})
		default:
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
	}))
	defer planque.Close()
	phone := testaccount.NewPhone(key, planque.URL, "tnt", planque.Client())
	ctx := context.Background()

	var out strings.Builder
	if err := disconnectGrant(ctx, phone, "grt_qwBr3staleone", &out); err != nil || deletes != 1 {
		t.Fatalf("the stale grant: %v, %d disconnects\n%s", err, deletes, out.String())
	}
	if !strings.Contains(out.String(), "grant grt_qwBr…") || !strings.Contains(out.String(), "now: disconnected") || strings.Contains(out.String(), "grt_qwBr3staleone") {
		t.Fatalf("the public log:\n%s", out.String())
	}
	out.Reset()
	if err := disconnectGrant(ctx, phone, "grt_gone0000000", &out); err != nil || deletes != 1 {
		t.Fatalf("an already disconnected grant was sent again: %v, %d disconnects", err, deletes)
	}
	if err := disconnectGrant(ctx, phone, "grt_otheraccount", &out); err == nil || deletes != 1 {
		t.Fatalf("another account's grant: %v, %d disconnects", err, deletes)
	}
}

// The one-off runs by hand only, in the test account's group (never beside
// an Eval or E2E run), with the phone's session and key and nothing else:
// no model key, and the grant passed as data, never into the shell.
func TestTheOneOffDisconnectRunsAloneAsThePhone(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "e2e-disconnect.yml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, must := range []string{"workflow_dispatch:", "group: test-account", "cancel-in-progress: false", "E2E_KIFF_SESSION", "E2E_PHONE_KEY",
		"GRANT: ${{ inputs.grant }}", `go run ./cmd/e2e -disconnect "$GRANT"`} {
		if !strings.Contains(s, must) {
			t.Fatalf("e2e-disconnect.yml lacks %q", must)
		}
	}
	for _, never := range []string{"schedule:", "ANTHROPIC_API_KEY", "-disconnect ${{"} {
		if strings.Contains(s, never) {
			t.Fatalf("e2e-disconnect.yml has %q", never)
		}
	}
}
