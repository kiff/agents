package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
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
	status := map[string]string{"grt_qwBr3staleone": "connected", "grt_gone0000000": "disconnected", "grt_twin1aaaa": "connected", "grt_twin2bbbb": "connected", "grt_failsWholeId": "connected"}
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
			// As planque: the disconnected ones only when asked for.
			var gs []map[string]string
			for id, st := range status {
				if st == "disconnected" && r.URL.Query().Get("include") != "disconnected" {
					continue
				}
				gs = append(gs, map[string]string{"id": id, "status": st})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"grants": gs})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/accounts/tnt/grants/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/accounts/tnt/grants/")
			if _, ok := status[id]; !ok {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			if id == "grt_failsWholeId" {
				// planque's words may name the whole id: they never reach the public log.
				http.Error(w, `{"error":"grant `+id+` can't be disconnected now"}`, http.StatusConflict)
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
	if err := disconnectGrant(ctx, phone, "grt_qwBr", &out); err != nil || deletes != 1 {
		t.Fatalf("the stale grant, by its first characters: %v, %d disconnects\n%s", err, deletes, out.String())
	}
	if !strings.Contains(out.String(), "grant grt_qwBr…") || !strings.Contains(out.String(), "now: disconnected") || strings.Contains(out.String(), "grt_qwBr3staleone") {
		t.Fatalf("the public log:\n%s", out.String())
	}
	out.Reset()
	if err := disconnectGrant(ctx, phone, "grt_gone", &out); err != nil || deletes != 1 {
		t.Fatalf("an already disconnected grant was sent again: %v, %d disconnects", err, deletes)
	}
	// planque refuses, naming the whole id: the error says only its first characters.
	err := disconnectGrant(ctx, phone, "grt_fail", &out)
	if err == nil || strings.Contains(err.Error(), "grt_failsWholeId") || !strings.Contains(err.Error(), "grt_fail…") {
		t.Fatalf("an error naming the whole id: %v", err)
	}
	for _, prefix := range []string{"grt_othe", "grt_twin", "grt_", "qwBr3sta", "grt_qwBr3staleone"} {
		if err := disconnectGrant(ctx, phone, prefix, &out); err == nil || deletes != 1 {
			t.Fatalf("%q (none, two, too short, not a grant, a whole id): %v, %d disconnects", prefix, err, deletes)
		}
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
	for _, must := range []string{"workflow_dispatch:", "group: test-account-2", "cancel-in-progress: false", "E2E_KIFF_SESSION", "E2E_PHONE_KEY",
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

// The run itself, from the phone's session: planque asks KIFF whether the
// device that signed is known (d= in the signature), so the phone names
// its device in KIFF, as the app and every run do. Without it planque
// answered 401 "this device isn't known to KIFF" and nothing was sent
// (agents' first dispatch of the one-off).
func TestTheDisconnectRunSignsAsThePhonesDevice(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	kiff := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"tenant_id": "tnt"})
		case "/v1/me/devices":
			_ = json.NewEncoder(w).Encode(map[string]any{"devices": []any{map[string]any{"id": "dev_other"}, map[string]any{"id": "dev_phone", "this_device": true}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer kiff.Close()
	status, deletes := "connected", 0
	planque := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Planque-Device-Signature"), ",d=dev_phone") {
			http.Error(w, `{"error":"this device isn't known to KIFF: sign in to KIFF on it again"}`, http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodDelete {
			deletes++
			status = "disconnected"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"grants": []any{map[string]string{"id": "grt_qwBr3staleone", "status": status}}})
	}))
	defer planque.Close()
	var out strings.Builder
	cfg := config{session: "session", phone: base64.StdEncoding.EncodeToString(der)}
	if err := runDisconnect(context.Background(), kiff.URL, planque.URL, cfg, "grt_qwBr", &out); err != nil || deletes != 1 || !strings.Contains(out.String(), "now: disconnected") {
		t.Fatalf("the run: %v, %d disconnects\n%s", err, deletes, out.String())
	}
}
