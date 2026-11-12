package testaccount

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeAccount is planque and KIFF as a fresh agent's steps meet them: the
// grant Setup asks for, the key the phone gives, the draft and its confirm,
// the key's claim, and the grant's disconnect and the agent's archive.
type fakeAccount struct {
	mu        sync.Mutex
	calls     []string
	asked     map[string]any
	draft     map[string]any
	keyStatus string // the grant's status after the key
	failClaim bool
}

func (f *fakeAccount) servers(t *testing.T) (planque, kiff *httptest.Server) {
	planque = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		f.calls = append(f.calls, "planque "+r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/accounts/tnt/grants":
			if r.Header.Get("Authorization") != "Bearer setup_key" {
				w.WriteHeader(401)
				return
			}
			_ = json.Unmarshal(b, &f.asked)
			_, _ = w.Write([]byte(`{"id":"grt_1","digest":"d1","status":"pending"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/accounts/tnt/grants/grt_1/key":
			if r.Header.Get("Planque-Vendor-Key") != "desk_key" || r.Header.Get("Planque-Device-Signature") == "" {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write([]byte(`{"status":"` + f.keyStatus + `"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/accounts/tnt/grants/grt_1":
			_, _ = w.Write([]byte(`{"status":"disconnected"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	kiff = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		f.calls = append(f.calls, "kiff "+r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/me/agent-drafts":
			if r.Header.Get("KIFF-Device-Signature") == "" {
				w.WriteHeader(403)
				return
			}
			_ = json.Unmarshal(b, &f.draft)
			_, _ = w.Write([]byte(`{"id":"drf_1","status":"pending","claim_secret":"kiff_claim_s"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/me/agent-drafts/drf_1":
			_, _ = w.Write([]byte(`{"id":"drf_1","version":"v7"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/me/agent-drafts/drf_1/confirm":
			if !strings.Contains(string(b), `"v7"`) || r.Header.Get("KIFF-Device-Signature") == "" {
				w.WriteHeader(409)
				return
			}
			_, _ = w.Write([]byte(`{"status":"confirmed"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/agent-drafts/drf_1/claim":
			if f.failClaim || r.Header.Get("Authorization") != "Bearer kiff_claim_s" {
				w.WriteHeader(409)
				return
			}
			_, _ = w.Write([]byte(`{"plaintext":"kiff_live_agent"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/archive"):
			_, _ = w.Write([]byte(`{"status":"archived"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(planque.Close)
	t.Cleanup(kiff.Close)
	return planque, kiff
}

func testAccount(t *testing.T, f *fakeAccount) Account {
	planque, kiff := f.servers(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p := NewPhone(key, planque.URL, "tnt", planque.Client())
	p.VendorKey = "desk_key"
	return Account{Phone: p, KIFF: KIFFSession(kiff.URL, "session", "tnt", key, kiff.Client()), Setup: SetupClient(planque.URL, "setup_key", planque.Client())}
}

var deskSpec = AgentSpec{ID: "eval-tickets-1a2b3c4d", Label: "Tickets (Eval)",
	Tools: []map[string]any{{"tool": "postReply", "name": "desk_reply"}},
	API:   map[string]any{"url": "https://vendor.example/desk/openapi.json"},
	Cards: []map[string]any{{"title": "Acknowledgements", "grants": []map[string]any{{"action": "desk_reply"}}}}}

// A fresh agent is made in order: Setup's grant for the new id, the phone's
// key, the phone's draft with the Cards held by the new agent, its confirm
// at the version read, and the key's claim. The sweep hears of the agent
// before anything can fail after the draft.
func TestAFreshAgentIsMadeInOrder(t *testing.T) {
	f := &fakeAccount{keyStatus: "connected"}
	var swept []string
	ag, err := testAccount(t, f).NewAgent(context.Background(), deskSpec, func(a string) error { swept = append(swept, a); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if ag.ID != "eval-tickets-1a2b3c4d" || ag.Key != "kiff_live_agent" || ag.Grant != "grt_1" {
		t.Fatalf("agent %+v", ag)
	}
	want := []string{"planque POST /v1/accounts/tnt/grants", "planque POST /v1/accounts/tnt/grants/grt_1/key", "kiff POST /v1/me/agent-drafts",
		"kiff GET /v1/me/agent-drafts/drf_1", "kiff POST /v1/me/agent-drafts/drf_1/confirm", "kiff POST /v1/agent-drafts/drf_1/claim"}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(f.calls, "\n"))
	}
	if f.asked["agent"] != ag.ID || f.asked["kind"] != "api_key" {
		t.Fatalf("asked %v", f.asked)
	}
	card := f.draft["cards"].([]any)[0].(map[string]any)
	if f.draft["agent_id"] != ag.ID || card["subject"] != ag.ID || card["title"] != "Acknowledgements" {
		t.Fatalf("draft %v", f.draft)
	}
	if len(swept) != 1 || swept[0] != ag.ID {
		t.Fatalf("swept %v", swept)
	}
	// The spec's own Cards are left as they were: the next run fills its own id.
	if _, ok := deskSpec.Cards[0]["subject"]; ok {
		t.Fatal("the spec's Card was changed")
	}
}

// A step that fails takes back what came before it: a grant that didn't
// connect, or a key that couldn't be claimed, leaves no grant or agent.
func TestAFailedStepTakesItAllBack(t *testing.T) {
	f := &fakeAccount{keyStatus: "listed"}
	if _, err := testAccount(t, f).NewAgent(context.Background(), deskSpec, nil); err == nil || !strings.Contains(err.Error(), "isn't connected") {
		t.Fatalf("a grant left listed: %v", err)
	}
	if last := f.calls[len(f.calls)-2:]; last[0] != "planque DELETE /v1/accounts/tnt/grants/grt_1" || !strings.HasPrefix(last[1], "kiff POST /v1/me/agents/eval-tickets-") {
		t.Fatalf("not taken back: %v", f.calls)
	}
	f = &fakeAccount{keyStatus: "connected", failClaim: true}
	if _, err := testAccount(t, f).NewAgent(context.Background(), deskSpec, nil); err == nil || !strings.Contains(err.Error(), "claiming") {
		t.Fatalf("a failed claim: %v", err)
	}
	if last := f.calls[len(f.calls)-2:]; last[0] != "planque DELETE /v1/accounts/tnt/grants/grt_1" || !strings.HasSuffix(last[1], "/archive") {
		t.Fatalf("not taken back: %v", f.calls)
	}
}

func TestARunIDIsShortAndItsOwn(t *testing.T) {
	a, err := RunID()
	b, _ := RunID()
	if err != nil || len(a) != 8 || a == b {
		t.Fatalf("%q %q %v", a, b, err)
	}
}
