package kiff_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/tool/kiff"
)

// allowanceAPI is KIFF's allowance route: it answers what it's set to, and
// counts the questions, each with the key it was asked with.
type allowanceAPI struct {
	mu     sync.Mutex
	status int
	body   string
	asked  int
	keys   []string
}

func (a *allowanceAPI) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path != "/v1/me/credits/allowance" {
			http.NotFound(w, r)
			return
		}
		a.asked++
		a.keys = append(a.keys, r.Header.Get("Authorization"))
		w.WriteHeader(a.status)
		_, _ = w.Write([]byte(a.body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (a *allowanceAPI) set(status int, body string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.status, a.body = status, body
}

// KIFF's answer, read: allowed, or refused with its code. A KIFF without the
// route allows; any other answer is an error, without the key.
func TestTheAllowanceAnswer(t *testing.T) {
	api := &allowanceAPI{}
	srv := api.server(t)
	for _, c := range []struct {
		status        int
		body          string
		allowed, fail bool
		why           string
	}{
		{200, `{"allowed": true}`, true, false, ""},
		{200, `{"allowed": false, "why": "limit_reached"}`, false, false, "limit_reached"},
		{404, `not found`, true, false, ""},
		{200, `{"allowed": false}`, false, true, ""},
		{200, `{}`, false, true, ""},
		{200, `nope`, false, true, ""},
		{403, `an agent's key asks for its own runs: ` + testKey, false, true, ""},
		{500, `{"error": "credits unavailable"}`, false, true, ""},
	} {
		api.set(c.status, c.body)
		allowed, why, err := kiff.Allowance(context.Background(), srv.Client(), srv.URL, testKey)
		if allowed != c.allowed || why != c.why || (err != nil) != c.fail {
			t.Errorf("%d %s: %v %q %v", c.status, c.body, allowed, why, err)
		}
		if err != nil && strings.Contains(err.Error(), testKey) {
			t.Errorf("the key in an error: %v", err)
		}
	}
	if api.keys[0] != "Bearer "+testKey {
		t.Fatalf("asked with %q", api.keys[0])
	}
}

// The checker asks with the agent's own key, and keeps an allowed answer a
// minute per agent; a refusal isn't kept, so the next run asks again and
// starts as soon as KIFF allows it.
func TestTheAllowanceChecker(t *testing.T) {
	api := &allowanceAPI{}
	srv := api.server(t)
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	check := kiff.AllowanceChecker(oneAgent{}, keys{}, srv.Client(), srv.URL, func() time.Time { return now })
	ctx := context.Background()

	api.set(200, `{"allowed": false, "why": "credits_used_up"}`)
	for range 2 {
		if ok, why, err := check(ctx, "t1", "rae"); ok || why != "credits_used_up" || err != nil {
			t.Fatalf("refused: %v %q %v", ok, why, err)
		}
	}
	// A pack, then: the next run starts at once.
	api.set(200, `{"allowed": true}`)
	if ok, _, err := check(ctx, "t1", "rae"); !ok || err != nil || api.asked != 3 {
		t.Fatalf("allowed after the refusals: %v %v, asked %d", ok, err, api.asked)
	}
	// Kept a minute: KIFF isn't asked again, even if it would refuse now.
	api.set(200, `{"allowed": false, "why": "credits_used_up"}`)
	now = now.Add(59 * time.Second)
	if ok, _, _ := check(ctx, "t1", "rae"); !ok || api.asked != 3 {
		t.Fatalf("within the minute: %v, asked %d", ok, api.asked)
	}
	now = now.Add(time.Second)
	if ok, why, _ := check(ctx, "t1", "rae"); ok || why != "credits_used_up" || api.asked != 4 {
		t.Fatalf("after the minute: %v %q, asked %d", ok, why, api.asked)
	}
	// An agent this account doesn't have, or a key it can't read, is an
	// error: the start fails, and KIFF isn't asked.
	if _, _, err := check(ctx, "t2", "rae"); err == nil || api.asked != 4 {
		t.Fatalf("another tenant's agent: %v, asked %d", err, api.asked)
	}
	// KIFF not answering is an error, never kept.
	api.set(502, `bad gateway`)
	if ok, _, err := check(ctx, "t1", "rae"); ok || err == nil {
		t.Fatalf("KIFF down: %v %v", ok, err)
	}
}
