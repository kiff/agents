package kiff

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
	"time"
)

// warmGateway is a fake gateway that opens a session per handshake (sess-1,
// sess-2…), requires it after, and counts each key's handshakes and lists.
// refuse answers 401 to a key; forget answers 404 to every session it gave.
type warmGateway struct {
	mu      sync.Mutex
	opened  int
	inits   map[string]int // by key
	lists   map[string]int // by key
	refuse  map[string]bool
	forget  bool
	session map[string]string // the key each session was opened for
}

func newWarmGateway(t *testing.T) (*warmGateway, *httptest.Server) {
	t.Helper()
	g := &warmGateway{inits: map[string]int{}, lists: map[string]int{}, refuse: map[string]bool{}, session: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		var msg struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &msg)
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.refuse[key] {
			http.Error(w, "this key was revoked", http.StatusUnauthorized)
			return
		}
		if msg.Method != "initialize" {
			sid := r.Header.Get("Mcp-Session-Id")
			if g.session[sid] != key || g.forget {
				// A session another key opened is never this key's.
				http.Error(w, "no such session", http.StatusNotFound)
				return
			}
		}
		if msg.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			g.opened++
			sid := fmt.Sprintf("sess-%d", g.opened)
			g.session[sid] = key
			g.inits[key]++
			g.forget = false
			w.Header().Set("Mcp-Session-Id", sid)
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
		case "tools/list":
			g.lists[key]++
			result = map[string]any{"tools": []any{map[string]any{"name": "stripe_get_charge", "inputSchema": map[string]any{"type": "object"}}}}
		}
		w.Header().Set("Content-Type", "application/json")
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return g, srv
}

func (g *warmGateway) counts(key string) (inits, lists int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inits[key], g.lists[key]
}

// A chat's next run of the same agent, with the same key, skips the
// handshake: one session, its tools listed afresh each run, so a Card that
// changed shows at once.
func TestAnAgentsNextRunReusesItsSession(t *testing.T) {
	g, srv := newWarmGateway(t)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	s := &Sessions{Now: func() time.Time { return now }}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := s.Tools(ctx, srv.URL, "t1", "coach", "kiff_key_coach"); err != nil {
			t.Fatal(err)
		}
		now = now.Add(30 * time.Second)
	}
	if inits, lists := g.counts("kiff_key_coach"); inits != 1 || lists != 3 {
		t.Fatalf("%d handshakes and %d lists for three runs, want 1 and 3", inits, lists)
	}
}

// Two agents of one tenant never share a session, nor do two tenants' agents
// of the same id: each opens its own, under its own key.
func TestTwoAgentsNeverShareASession(t *testing.T) {
	g, srv := newWarmGateway(t)
	s := &Sessions{}
	ctx := context.Background()
	for _, run := range []struct{ tenant, agent, key string }{
		{"t1", "coach", "kiff_key_coach"}, {"t1", "refunds", "kiff_key_refunds"}, {"t2", "coach", "kiff_key_other"},
		{"t1", "coach", "kiff_key_coach"}, {"t1", "refunds", "kiff_key_refunds"}, {"t2", "coach", "kiff_key_other"},
	} {
		if _, err := s.Tools(ctx, srv.URL, run.tenant, run.agent, run.key); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"kiff_key_coach", "kiff_key_refunds", "kiff_key_other"} {
		if inits, lists := g.counts(key); inits != 1 || lists != 2 {
			t.Fatalf("%s: %d handshakes and %d lists, want 1 and 2", key, inits, lists)
		}
	}
	coach, _ := s.client(srv.URL, "t1", "coach", "kiff_key_coach")
	refunds, _ := s.client(srv.URL, "t1", "refunds", "kiff_key_refunds")
	other, _ := s.client(srv.URL, "t2", "coach", "kiff_key_other")
	if coach == refunds || coach == other || refunds == other {
		t.Fatal("two agents were given the same session")
	}
	// The same key under another agent still opens its own.
	if borrowed, warm := s.client(srv.URL, "t1", "refunds", "kiff_key_coach"); warm || borrowed == coach {
		t.Fatal("an agent was given another agent's session for the same key")
	}
}

// A new key for the agent (a rotation) replaces its session; the old one is
// never used again.
func TestARotatedKeyOpensANewSession(t *testing.T) {
	g, srv := newWarmGateway(t)
	s := &Sessions{}
	ctx := context.Background()
	if _, err := s.Tools(ctx, srv.URL, "t1", "coach", "kiff_key_old"); err != nil {
		t.Fatal(err)
	}
	old, _ := s.client(srv.URL, "t1", "coach", "kiff_key_old")
	if _, err := s.Tools(ctx, srv.URL, "t1", "coach", "kiff_key_new"); err != nil {
		t.Fatal(err)
	}
	if inits, _ := g.counts("kiff_key_new"); inits != 1 {
		t.Fatalf("the new key opened %d sessions, want 1", inits)
	}
	if again, warm := s.client(srv.URL, "t1", "coach", "kiff_key_old"); warm || again == old {
		t.Fatal("the rotated key's session was kept")
	}
}

// A refused answer drops the session. A revoked key fails the next start as
// it would have; a session the gateway forgot is opened again, once, and the
// start goes on.
func TestARefusedSessionIsDropped(t *testing.T) {
	g, srv := newWarmGateway(t)
	s := &Sessions{}
	ctx := context.Background()
	if _, err := s.Tools(ctx, srv.URL, "t1", "coach", "kiff_key_coach"); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	g.forget = true
	g.mu.Unlock()
	if _, err := s.Tools(ctx, srv.URL, "t1", "coach", "kiff_key_coach"); err != nil {
		t.Fatalf("a forgotten session failed the start: %v", err)
	}
	if inits, lists := g.counts("kiff_key_coach"); inits != 2 || lists != 2 {
		t.Fatalf("%d handshakes and %d lists, want a new session and the list once more", inits, lists)
	}
	warm, _ := s.client(srv.URL, "t1", "coach", "kiff_key_coach")
	g.mu.Lock()
	g.refuse["kiff_key_coach"] = true
	g.mu.Unlock()
	if _, err := warm.Call(ctx, "stripe_get_charge", json.RawMessage(`{}`)); err == nil {
		t.Fatal("a revoked key's call went through")
	}
	if again, reused := s.client(srv.URL, "t1", "coach", "kiff_key_coach"); reused || again == warm {
		t.Fatal("a refused session was kept")
	}
	if _, err := s.Tools(ctx, srv.URL, "t1", "coach", "kiff_key_coach"); err == nil {
		t.Fatal("a revoked key started")
	}
}

// A session is never reused once SessionTTL has passed since its handshake,
// however often it was used, and the key is never kept, only its SHA-256.
func TestASessionNeverOutlivesItsTime(t *testing.T) {
	g, srv := newWarmGateway(t)
	start := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	now := start
	s := &Sessions{Now: func() time.Time { return now }}
	ctx := context.Background()
	for _, at := range []time.Duration{0, time.Minute, SessionTTL - time.Second, SessionTTL} {
		now = start.Add(at)
		if _, err := s.Tools(ctx, srv.URL, "t1", "coach", "kiff_key_coach"); err != nil {
			t.Fatal(err)
		}
	}
	if inits, _ := g.counts("kiff_key_coach"); inits != 2 {
		t.Fatalf("%d handshakes, want a second one at %s", inits, SessionTTL)
	}
	// A clock that went back opens a new one too.
	now = start.Add(-time.Minute)
	if _, warm := s.client(srv.URL, "t1", "coach", "kiff_key_coach"); warm {
		t.Fatal("a session from the future was reused")
	}
	s.mu.Lock()
	kept := fmt.Sprintf("%+v", s.m)
	s.mu.Unlock()
	if strings.Contains(kept, "kiff_key_coach") {
		t.Fatalf("the sessions keep the key: %s", kept)
	}
}

// Runs of one agent at once share its session safely.
func TestRunsAtOnceShareTheirAgentsSession(t *testing.T) {
	g, srv := newWarmGateway(t)
	s := &Sessions{}
	ctx := context.Background()
	if _, err := s.Tools(ctx, srv.URL, "t1", "coach", "kiff_key_coach"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Tools(ctx, srv.URL, "t1", "coach", "kiff_key_coach"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if inits, lists := g.counts("kiff_key_coach"); inits != 1 || lists != 9 {
		t.Fatalf("%d handshakes and %d lists, want 1 and 9", inits, lists)
	}
}
