package kiff_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiff/agents/tool/kiff"
)

// #221: the runtime asks KIFF what a person may do with an agent, with the
// agent's own key, and keeps each answer a minute; KIFF not answering is
// no answer, which isn't kept.
func TestAccessorAsksWithTheAgentsKeyAndKeepsAMinute(t *testing.T) {
	var asked atomic.Int32
	var down atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/me/access" || r.Header.Get("Authorization") != "Bearer "+testKey {
			http.Error(w, `{"error":"no"}`, http.StatusUnauthorized)
			return
		}
		asked.Add(1)
		if down.Load() {
			http.Error(w, `{"error":"access failed"}`, http.StatusInternalServerError)
			return
		}
		var body struct{ Subject string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{"use": body.Subject == "ana", "oversee": false, "manage": false, "groups": []string{}, "checked_at": "2026-10-25T12:00:00Z"})
	}))
	t.Cleanup(srv.Close)
	clock := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	access := kiff.Accessor(oneAgent{}, keys{}, srv.Client(), srv.URL, func() time.Time { return clock })
	ctx := context.Background()

	if a, err := access(ctx, "t1", "rae", "ana"); err != nil || !a.Use {
		t.Fatalf("ana: %+v, %v", a, err)
	}
	if a, err := access(ctx, "t1", "rae", "bo"); err != nil || a.Use {
		t.Fatalf("bo: %+v, %v", a, err)
	}
	if _, err := access(ctx, "t1", "rae", "ana"); err != nil || asked.Load() != 2 {
		t.Fatalf("ana again within the minute asked KIFF: %d asks, %v", asked.Load(), err)
	}
	clock = clock.Add(kiff.AccessTTL)
	down.Store(true)
	if _, err := access(ctx, "t1", "rae", "ana"); err == nil {
		t.Fatal("KIFF down after the minute: an answer, want none")
	}
	down.Store(false)
	if a, err := access(ctx, "t1", "rae", "ana"); err != nil || !a.Use || asked.Load() != 4 {
		t.Fatalf("ana once KIFF is back: %+v, %v (%d asks)", a, err, asked.Load())
	}
}

// Asked afresh, KIFF is asked every time, and cards_manage is read only
// when KIFF says it: missing stays unknown, never no or yes.
func TestFreshAccessAsksEachTimeAndReadsCardsManage(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		var body struct{ Subject string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		out := map[string]any{"use": true}
		if body.Subject == "ana" {
			out["cards_manage"] = true
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	access := kiff.FreshAccessor(oneAgent{}, keys{}, srv.Client(), srv.URL)
	ctx := context.Background()
	for range 2 {
		if a, err := access(ctx, "t1", "rae", "ana"); err != nil || a.CardsManage == nil || !*a.CardsManage {
			t.Fatalf("ana: %+v, %v", a, err)
		}
	}
	if a, err := access(ctx, "t1", "rae", "bo"); err != nil || a.CardsManage != nil {
		t.Fatalf("bo: %+v, %v; want cards_manage unknown", a, err)
	}
	if asked.Load() != 3 {
		t.Fatalf("KIFF asked %d times, want 3", asked.Load())
	}
}
