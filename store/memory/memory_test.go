package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/kiff/agents/session"
	"github.com/kiff/agents/session/sessiontest"
	"github.com/kiff/agents/store/memory"
)

func TestStore(t *testing.T) {
	sessiontest.Run(t, func(_ *testing.T, now func() time.Time) sessiontest.Backend { return memory.NewAt(now) })
}

// The memory store has no expiry of its own: Purge removes what has expired,
// sessions included, and counts the events.
func TestPurgeRemoves(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	m := memory.NewAt(func() time.Time { return at })
	old := session.Session{ID: "old", Tenant: "t1", Agent: "a", Expires: at.Add(time.Hour)}
	live := session.Session{ID: "live", Tenant: "t1", Agent: "a"}
	for _, s := range []session.Session{old, live} {
		if err := m.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	ev := func(seq int64, expires time.Time) session.Event {
		return session.Event{Seq: seq, Time: at, Kind: session.Report, Body: []byte(`1`), Expires: expires}
	}
	for _, id := range []string{"old", "live"} {
		if err := m.Begin(ctx, "t1", id, "r1", at); err != nil {
			t.Fatal(err)
		}
	}
	_ = m.Append(ctx, "t1", "old", "r1", []session.Event{ev(1, at.Add(time.Hour)), ev(2, at.Add(time.Hour))})
	_ = m.Append(ctx, "t1", "live", "r1", []session.Event{ev(1, at.Add(time.Hour)), ev(2, time.Time{})})

	n, err := m.Purge(ctx, at.Add(2*time.Hour))
	if err != nil || n != 3 {
		t.Fatalf("purged %d events (err %v), want 3: two with the old session, one expired in the live one", n, err)
	}
	if n, _ := m.Purge(ctx, at.Add(2*time.Hour)); n != 0 {
		t.Fatalf("a second purge removed %d", n)
	}
}
