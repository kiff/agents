// Package attachtest checks that an attach.Store keeps the promises the
// attach package makes. Every store runs the same checks:
//
//	func TestAttachments(t *testing.T) {
//		attachtest.Run(t, func(t *testing.T) attach.Store { return newStore(t) })
//	}
package attachtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kiff/agents/attach"
)

// Run runs every check, each against a fresh store from newStore.
func Run(t *testing.T, newStore func(t *testing.T) attach.Store) {
	checks := []struct {
		name string
		fn   func(*testing.T, attach.Store)
	}{
		{"CreateAndGet", createAndGet},
		{"OnlyTheTenantReadsIt", onlyTheTenantReadsIt},
		{"BoundOnce", boundOnce},
		{"ListedBySessionAndAgent", listedBySessionAndAgent},
		{"DueWhenAbandonedOrExpired", dueWhenAbandonedOrExpired},
		{"DeleteIsIdempotent", deleteIsIdempotent},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) { c.fn(t, newStore(t)) })
	}
}

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func att(id, agent string) attach.Attachment {
	return attach.Attachment{ID: id, Tenant: "t1", Agent: agent, Owner: "member:ana", MediaType: "image/png",
		Name: "receipt.png", Size: 10, SHA256: "ab", Created: t0}
}

func createAndGet(t *testing.T, s attach.Store) {
	ctx := context.Background()
	a := att("att_1", "refunds")
	if err := s.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "t1", "att_1")
	if err != nil || got.ID != a.ID || got.Owner != a.Owner || got.Size != a.Size || got.MediaType != a.MediaType || !got.Created.Equal(t0) || got.Checked {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if err := s.Check(ctx, "t1", "att_1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, "t1", "att_1"); !got.Checked {
		t.Fatal("not checked after Check")
	}
	if _, err := s.Get(ctx, "t1", "att_2"); !errors.Is(err, attach.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func onlyTheTenantReadsIt(t *testing.T, s attach.Store) {
	ctx := context.Background()
	if err := s.Create(ctx, att("att_1", "refunds")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "t2", "att_1"); !errors.Is(err, attach.ErrNotFound) {
		t.Fatalf("another tenant read it: %v", err)
	}
	if err := s.Bind(ctx, "t2", "att_1", "s1", "r1", t0, time.Time{}); !errors.Is(err, attach.ErrNotFound) {
		t.Fatalf("another tenant bound it: %v", err)
	}
}

func boundOnce(t *testing.T, s attach.Store) {
	ctx := context.Background()
	if err := s.Create(ctx, att("att_1", "refunds")); err != nil {
		t.Fatal(err)
	}
	exp := t0.Add(30 * 24 * time.Hour)
	if err := s.Bind(ctx, "t1", "att_1", "s1", "r1", t0.Add(time.Minute), exp); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, "t1", "att_1")
	if got.Session != "s1" || got.Run != "r1" || !got.Expires.Equal(exp) || !got.Bound.Equal(t0.Add(time.Minute)) {
		t.Fatalf("bound = %+v", got)
	}
	if err := s.Bind(ctx, "t1", "att_1", "s2", "r2", t0, exp); !errors.Is(err, attach.ErrBound) {
		t.Fatalf("bound twice: %v", err)
	}
	if err := s.Bind(ctx, "t1", "att_9", "s1", "r1", t0, exp); !errors.Is(err, attach.ErrNotFound) {
		t.Fatalf("bound a missing one: %v", err)
	}
}

func listedBySessionAndAgent(t *testing.T, s attach.Store) {
	ctx := context.Background()
	for _, a := range []attach.Attachment{att("att_1", "refunds"), att("att_2", "refunds"), att("att_3", "bills")} {
		if err := s.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Bind(ctx, "t1", "att_1", "s1", "r1", t0, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if as, err := s.Session(ctx, "t1", "s1"); err != nil || len(as) != 1 || as[0].ID != "att_1" {
		t.Fatalf("Session = %v, %v", as, err)
	}
	if as, err := s.Agent(ctx, "t1", "refunds"); err != nil || len(as) != 2 {
		t.Fatalf("Agent = %v, %v", as, err)
	}
	if as, err := s.Agent(ctx, "t2", "refunds"); err != nil || len(as) != 0 {
		t.Fatalf("another tenant's agent = %v, %v", as, err)
	}
}

func dueWhenAbandonedOrExpired(t *testing.T, s attach.Store) {
	ctx := context.Background()
	for _, a := range []attach.Attachment{att("att_1", "refunds"), att("att_2", "refunds"), att("att_3", "refunds")} {
		if err := s.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	// att_1 never sent; att_2 sent, going in 30 days; att_3 sent, kept with its session.
	if err := s.Bind(ctx, "t1", "att_2", "s1", "r1", t0, t0.Add(30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Bind(ctx, "t1", "att_3", "s1", "r1", t0, time.Time{}); err != nil {
		t.Fatal(err)
	}
	ids := func(now time.Time) []string {
		as, err := s.Due(ctx, now, 10)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, a := range as {
			out = append(out, a.ID)
		}
		return out
	}
	if got := ids(t0.Add(59 * time.Minute)); len(got) != 0 {
		t.Fatalf("due before an hour: %v", got)
	}
	if got := ids(t0.Add(61 * time.Minute)); len(got) != 1 || got[0] != "att_1" {
		t.Fatalf("due after an hour: %v", got)
	}
	if got := ids(t0.Add(31 * 24 * time.Hour)); len(got) != 2 || got[0] != "att_1" || got[1] != "att_2" {
		t.Fatalf("due after 31 days: %v", got)
	}
	if as, _ := s.Due(ctx, t0.Add(31*24*time.Hour), 1); len(as) != 1 {
		t.Fatalf("limit not kept: %d", len(as))
	}
}

func deleteIsIdempotent(t *testing.T, s attach.Store) {
	ctx := context.Background()
	if err := s.Create(ctx, att("att_1", "refunds")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.Delete(ctx, "t1", "att_1"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Get(ctx, "t1", "att_1"); !errors.Is(err, attach.ErrNotFound) {
		t.Fatalf("after Delete: %v", err)
	}
	if as, _ := s.Due(ctx, t0.Add(2*time.Hour), 10); len(as) != 0 {
		t.Fatalf("a deleted one is due: %v", as)
	}
}
