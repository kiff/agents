// Package sessiontest checks that a session.Store and session.Notes keep the
// promises the session package makes. Every backend runs the same checks:
//
//	func TestStore(t *testing.T) { sessiontest.Run(t, func() sessiontest.Backend { return newBackend(t) }) }
package sessiontest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kiff/agents/session"
)

// Backend is what a store under test provides.
type Backend interface {
	session.Store
	session.Notes
}

// Run runs every check against fresh backends from newBackend.
func Run(t *testing.T, newBackend func() Backend) {
	t.Run("RecordAndRead", func(t *testing.T) { recordAndRead(t, newBackend()) })
	t.Run("NeverIsNotWritten", func(t *testing.T) { neverIsNotWritten(t, newBackend()) })
	t.Run("UntilEndIsDroppedAtEnd", func(t *testing.T) { untilEndIsDroppedAtEnd(t, newBackend()) })
	t.Run("EndedTakesNoEvents", func(t *testing.T) { endedTakesNoEvents(t, newBackend()) })
	t.Run("PurgeRemovesExpired", func(t *testing.T) { purgeRemovesExpired(t, newBackend()) })
	t.Run("RemoveLeavesNothing", func(t *testing.T) { removeLeavesNothing(t, newBackend()) })
	t.Run("TenantsAreApart", func(t *testing.T) { tenantsAreApart(t, newBackend()) })
	t.Run("ListNewestFirst", func(t *testing.T) { listNewestFirst(t, newBackend()) })
	t.Run("Notes", func(t *testing.T) { notes(t, newBackend()) })
}

// clock is a time that moves only when told.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func start() *clock                  { return &clock{t: time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)} }
func recorder(t *testing.T, b Backend, p session.Policy, c *clock) *session.Recorder {
	t.Helper()
	r, err := session.NewRecorderAt(b, p, c.now)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func begin(t *testing.T, r *session.Recorder, agent string, kind session.Kind) session.Session {
	t.Helper()
	s, err := r.Start(context.Background(), "t1", agent, kind)
	check(t, err)
	return s
}

func list(t *testing.T, b Backend, tenant string, limit int) []session.Session {
	t.Helper()
	l, err := b.List(context.Background(), tenant, "refunds", limit)
	check(t, err)
	return l
}

func listNotes(t *testing.T, b Backend, tenant, agent string) []session.Note {
	t.Helper()
	n, err := b.ListNotes(context.Background(), tenant, agent)
	check(t, err)
	return n
}

func purge(t *testing.T, b Backend, now time.Time) int {
	t.Helper()
	n, err := b.Purge(context.Background(), now)
	check(t, err)
	return n
}

func record(t *testing.T, r *session.Recorder, s session.Session, k session.EventKind, body any) {
	t.Helper()
	if _, _, err := r.Record(context.Background(), s, k, body); err != nil {
		t.Fatal(err)
	}
}

func kinds(events []session.Event) []session.EventKind {
	var out []session.EventKind
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

func equal(a, b []session.EventKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func recordAndRead(t *testing.T, b Backend) {
	ctx, c := context.Background(), start()
	r := recorder(t, b, session.TafsDefault(30*24*time.Hour), c)
	s := begin(t, r, "refunds", session.Chat)
	record(t, r, s, session.OwnerMessage, map[string]string{"text": "refund order 12"})
	c.add(time.Second)
	record(t, r, s, session.AgentMessage, map[string]string{"text": "done"})
	record(t, r, s, session.Usage, session.UsageBody{Model: "m", InputTokens: 10, OutputTokens: 5, CostMicroUSD: 1200})

	got, events, err := b.Read(ctx, "t1", s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != session.Running || got.Agent != "refunds" || got.Kind != session.Chat {
		t.Fatalf("session = %+v", got)
	}
	if want := []session.EventKind{session.OwnerMessage, session.AgentMessage, session.Usage}; !equal(kinds(events), want) {
		t.Fatalf("kinds = %v, want %v", kinds(events), want)
	}
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d", i, e.Seq)
		}
	}
	if !got.Updated.Equal(c.now()) {
		t.Fatalf("updated = %v, want %v", got.Updated, c.now())
	}
	if cost, err := session.Cost(events); err != nil || cost != 1200 {
		t.Fatalf("cost = %d, err = %v", cost, err)
	}
}

func neverIsNotWritten(t *testing.T, b Backend) {
	ctx, c := context.Background(), start()
	p := session.TafsDefault(0)
	p[session.ToolResult] = session.Rule{Keep: session.Never}
	r := recorder(t, b, p, c)
	s := begin(t, r, "refunds", session.Run)
	_, written, err := r.Record(ctx, s, session.ToolResult, map[string]string{"customer": "someone"})
	if err != nil || written {
		t.Fatalf("written = %v, err = %v", written, err)
	}
	_, events, _ := b.Read(ctx, "t1", s.ID)
	if len(events) != 0 {
		t.Fatalf("a Never event was stored: %v", kinds(events))
	}
}

func untilEndIsDroppedAtEnd(t *testing.T, b Backend) {
	ctx, c := context.Background(), start()
	r := recorder(t, b, session.TafsDefault(0), c)
	s := begin(t, r, "refunds", session.Run)
	record(t, r, s, session.ToolCall, map[string]string{"tool": "list_charges"})
	record(t, r, s, session.ToolResult, map[string]string{"charges": "…"})
	record(t, r, s, session.Report, map[string]string{"text": "refunded 1"})
	_, during, _ := b.Read(ctx, "t1", s.ID)
	if len(during) != 3 {
		t.Fatalf("during the run, %d events, want 3: the agent still needs the result", len(during))
	}
	if err := r.End(ctx, s, session.Ended); err != nil {
		t.Fatal(err)
	}
	got, after, _ := b.Read(ctx, "t1", s.ID)
	if want := []session.EventKind{session.ToolCall, session.Report}; !equal(kinds(after), want) {
		t.Fatalf("after the end, kinds = %v, want %v", kinds(after), want)
	}
	if got.Status != session.Ended {
		t.Fatalf("status = %s", got.Status)
	}
}

func endedTakesNoEvents(t *testing.T, b Backend) {
	ctx, c := context.Background(), start()
	r := recorder(t, b, session.TafsDefault(0), c)
	s := begin(t, r, "refunds", session.Run)
	if err := r.End(ctx, s, session.Failed); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Record(ctx, s, session.AgentMessage, "late"); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("recording after the end: err = %v, want ErrEnded", err)
	}
	if err := r.End(ctx, s, session.Ended); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("ending twice: err = %v, want ErrEnded", err)
	}
}

func purgeRemovesExpired(t *testing.T, b Backend) {
	ctx, c := context.Background(), start()
	p := session.TafsDefault(30 * 24 * time.Hour)
	p[session.Report] = session.Rule{Keep: session.Kept} // no retention: lives as long as the session
	r := recorder(t, b, p, c)
	s := begin(t, r, "refunds", session.Run)
	record(t, r, s, session.OwnerMessage, "old")
	c.add(20 * 24 * time.Hour)
	record(t, r, s, session.AgentMessage, "newer")
	record(t, r, s, session.Report, "report")

	if n := purge(t, b, c.now()); n != 0 {
		t.Fatalf("purged %d before anything expired", n)
	}
	c.add(15 * 24 * time.Hour) // the first event is 35 days old, the others 15
	if n := purge(t, b, c.now()); n != 1 {
		t.Fatalf("purged %d, want 1", n)
	}
	_, events, _ := b.Read(ctx, "t1", s.ID)
	if want := []session.EventKind{session.AgentMessage, session.Report}; !equal(kinds(events), want) {
		t.Fatalf("kinds = %v, want %v", kinds(events), want)
	}
}

func removeLeavesNothing(t *testing.T, b Backend) {
	ctx, c := context.Background(), start()
	r := recorder(t, b, session.TafsDefault(0), c)
	gone := begin(t, r, "refunds", session.Run)
	record(t, r, gone, session.Report, "x")
	kept := begin(t, r, "chargebacks", session.Run)
	check(t, b.PutNote(ctx, session.Note{Tenant: "t1", Agent: "refunds", Name: "a", Text: "x"}))
	check(t, b.PutNote(ctx, session.Note{Tenant: "t1", Agent: "chargebacks", Name: "a", Text: "y"}))

	if err := session.Remove(ctx, b, b, "t1", "refunds"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Read(ctx, "t1", gone.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("removed session read: err = %v", err)
	}
	if l := list(t, b, "t1", 0); len(l) != 0 {
		t.Fatalf("removed agent still lists %d sessions", len(l))
	}
	if n := listNotes(t, b, "t1", "refunds"); len(n) != 0 {
		t.Fatalf("removed agent still has %d notes", len(n))
	}
	if _, _, err := b.Read(ctx, "t1", kept.ID); err != nil {
		t.Fatalf("another agent's session went too: %v", err)
	}
	if n := listNotes(t, b, "t1", "chargebacks"); len(n) != 1 {
		t.Fatalf("another agent's notes went too")
	}
}

func tenantsAreApart(t *testing.T, b Backend) {
	ctx, c := context.Background(), start()
	r := recorder(t, b, session.TafsDefault(0), c)
	s := begin(t, r, "refunds", session.Run)
	other := session.Session{ID: s.ID, Tenant: "t2", Agent: "refunds"}
	if _, _, err := b.Read(ctx, "t2", s.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("t2 read t1's session: err = %v", err)
	}
	if _, _, err := r.Record(ctx, other, session.AgentMessage, "x"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("t2 wrote to t1's session: err = %v", err)
	}
	if err := b.End(ctx, "t2", s.ID, session.Ended); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("t2 ended t1's session: err = %v", err)
	}
	if err := b.Delete(ctx, "t2", s.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("t2 deleted t1's session: err = %v", err)
	}
	check(t, b.DeleteAgent(ctx, "t2", "refunds"))
	if l := list(t, b, "t2", 0); len(l) != 0 {
		t.Fatalf("t2 lists t1's sessions")
	}
	if _, _, err := b.Read(ctx, "t1", s.ID); err != nil {
		t.Fatalf("t1's session is gone after t2's calls: %v", err)
	}
}

func listNewestFirst(t *testing.T, b Backend) {
	ctx, c := context.Background(), start()
	r := recorder(t, b, session.TafsDefault(0), c)
	first := begin(t, r, "refunds", session.Run)
	c.add(time.Minute)
	second := begin(t, r, "refunds", session.Chat)
	c.add(time.Minute)
	record(t, r, first, session.Report, "x") // first is now the latest updated
	l := list(t, b, "t1", 0)
	if len(l) != 2 || l[0].ID != first.ID || l[1].ID != second.ID {
		t.Fatalf("list = %+v", l)
	}
	if l := list(t, b, "t1", 1); len(l) != 1 || l[0].ID != first.ID {
		t.Fatalf("limit 1: %+v", l)
	}
	check(t, b.Delete(ctx, "t1", first.ID))
	if l := list(t, b, "t1", 0); len(l) != 1 || l[0].ID != second.ID {
		t.Fatalf("after delete: %+v", l)
	}
}

func notes(t *testing.T, b Backend) {
	ctx := context.Background()
	put := func(name, text string) {
		check(t, b.PutNote(ctx, session.Note{Tenant: "t1", Agent: "refunds", Name: name, Text: text}))
	}
	put("b", "1")
	put("a", "2")
	put("b", "3")
	n := listNotes(t, b, "t1", "refunds")
	if len(n) != 2 || n[0].Name != "a" || n[1].Text != "3" {
		t.Fatalf("notes = %+v", n)
	}
	check(t, b.DeleteNote(ctx, "t1", "refunds", "a"))
	check(t, b.DeleteNote(ctx, "t1", "refunds", "missing"))
	if n := listNotes(t, b, "t2", "refunds"); len(n) != 0 {
		t.Fatalf("t2 sees t1's notes")
	}
	if n := listNotes(t, b, "t1", "refunds"); len(n) != 1 || n[0].Name != "b" {
		t.Fatalf("after forgetting: %+v", n)
	}
}
