// Package sessiontest checks that a session.Store and session.Notes keep the
// promises the session package makes. Every backend runs the same checks:
//
//	func TestStore(t *testing.T) { sessiontest.Run(t, func() sessiontest.Backend { return newBackend(t) }) }
package sessiontest

import (
	"context"
	"errors"
	"slices"
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
	t.Run("ConcurrentRecordsAllLand", func(t *testing.T) { concurrentRecordsAllLand(t, newBackend()) })
	t.Run("Notes", func(t *testing.T) { notes(t, newBackend()) })
}

var ctx = context.Background()

// clock is a time that moves only when told.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func start() *clock                  { return &clock{t: time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)} }

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func recorder(t *testing.T, b Backend, p session.Policy, c *clock) *session.Recorder {
	t.Helper()
	r, err := session.NewRecorderAt(b, p, c.now)
	check(t, err)
	return r
}

// begin starts a recording that is ended when the test ends, if it wasn't.
func begin(t *testing.T, r *session.Recorder, agent string, kind session.Kind) *session.Recording {
	t.Helper()
	rec, err := r.Start(ctx, "t1", agent, kind)
	check(t, err)
	t.Cleanup(func() { _ = rec.End(ctx, session.Ended) })
	return rec
}

func record(t *testing.T, rec *session.Recording, k session.EventKind, body any) {
	t.Helper()
	check(t, rec.Record(k, body))
}

// read flushes the recording and reads its session back.
func read(t *testing.T, b Backend, rec *session.Recording) (session.Session, []session.Event) {
	t.Helper()
	if err := rec.Flush(ctx); err != nil && !errors.Is(err, session.ErrEnded) {
		t.Fatal(err)
	}
	s, events, err := b.Read(ctx, "t1", rec.Session().ID)
	check(t, err)
	return s, events
}

func list(t *testing.T, b Backend, tenant string, limit int) []session.Session {
	t.Helper()
	l, err := b.List(ctx, tenant, "refunds", limit)
	check(t, err)
	return l
}

func listNotes(t *testing.T, b Backend, tenant, agent string) []session.Note {
	t.Helper()
	n, err := b.ListNotes(ctx, tenant, agent)
	check(t, err)
	return n
}

func purge(t *testing.T, b Backend, now time.Time) int {
	t.Helper()
	n, err := b.Purge(ctx, now)
	check(t, err)
	return n
}

func kinds(events []session.Event) []session.EventKind {
	var out []session.EventKind
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

func recordAndRead(t *testing.T, b Backend) {
	c := start()
	rec := begin(t, recorder(t, b, session.DefaultPolicy(30*24*time.Hour), c), "refunds", session.KindChat)
	record(t, rec, session.OwnerMessage, map[string]string{"text": "refund order 12"})
	c.add(time.Second)
	record(t, rec, session.AgentMessage, map[string]string{"text": "done"})
	record(t, rec, session.Usage, session.UsageBody{Model: "m", InputTokens: 10, OutputTokens: 5, CostMicroUSD: 1200})

	got, events := read(t, b, rec)
	if got.Status != session.Running || got.Agent != "refunds" || got.Kind != session.KindChat {
		t.Fatalf("session = %+v", got)
	}
	if want := []session.EventKind{session.OwnerMessage, session.AgentMessage, session.Usage}; !slices.Equal(kinds(events), want) {
		t.Fatalf("kinds = %v, want %v", kinds(events), want)
	}
	for i, e := range events {
		if e.Seq != int64(i+1) || e.Session != got.ID {
			t.Fatalf("event %d: seq %d, session %q", i, e.Seq, e.Session)
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
	p := session.DefaultPolicy(0)
	p[session.ToolResult] = session.Rule{Keep: session.Never}
	rec := begin(t, recorder(t, b, p, start()), "refunds", session.KindRun)
	record(t, rec, session.ToolResult, map[string]string{"customer": "someone"})
	if _, events := read(t, b, rec); len(events) != 0 {
		t.Fatalf("a Never event was stored: %v", kinds(events))
	}
}

func untilEndIsDroppedAtEnd(t *testing.T, b Backend) {
	rec := begin(t, recorder(t, b, session.DefaultPolicy(0), start()), "refunds", session.KindRun)
	record(t, rec, session.ToolCall, map[string]string{"tool": "list_charges"})
	record(t, rec, session.ToolResult, map[string]string{"charges": "…"})
	record(t, rec, session.Report, map[string]string{"text": "refunded 1"})
	if _, during := read(t, b, rec); len(during) != 3 {
		t.Fatalf("during the run, %d events, want 3: the agent still needs the result", len(during))
	}
	check(t, rec.End(ctx, session.Ended))
	got, after := read(t, b, rec)
	if want := []session.EventKind{session.ToolCall, session.Report}; !slices.Equal(kinds(after), want) {
		t.Fatalf("after the end, kinds = %v, want %v", kinds(after), want)
	}
	if got.Status != session.Ended {
		t.Fatalf("status = %s", got.Status)
	}
}

func endedTakesNoEvents(t *testing.T, b Backend) {
	rec := begin(t, recorder(t, b, session.DefaultPolicy(0), start()), "refunds", session.KindRun)
	check(t, rec.End(ctx, session.Failed))
	if err := rec.Record(session.AgentMessage, "late"); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("recording after the end: err = %v, want ErrEnded", err)
	}
	if err := rec.End(ctx, session.Ended); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("ending twice: err = %v, want ErrEnded", err)
	}
	if err := b.Append(ctx, "t1", rec.Session().ID, []session.Event{{Kind: session.AgentMessage}}); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("appending to an ended session: err = %v, want ErrEnded", err)
	}
	if s, _ := read(t, b, rec); s.Status != session.Failed {
		t.Fatalf("status = %s, want failed", s.Status)
	}
}

func purgeRemovesExpired(t *testing.T, b Backend) {
	c := start()
	p := session.DefaultPolicy(30 * 24 * time.Hour)
	p[session.Report] = session.Rule{Keep: session.Kept} // no retention: lives as long as the session
	rec := begin(t, recorder(t, b, p, c), "refunds", session.KindRun)
	record(t, rec, session.OwnerMessage, "old")
	c.add(20 * 24 * time.Hour)
	record(t, rec, session.AgentMessage, "newer")
	record(t, rec, session.Report, "report")
	read(t, b, rec)

	if n := purge(t, b, c.now()); n != 0 {
		t.Fatalf("purged %d before anything expired", n)
	}
	c.add(15 * 24 * time.Hour) // the first event is 35 days old, the others 15
	if n := purge(t, b, c.now()); n != 1 {
		t.Fatalf("purged %d, want 1", n)
	}
	if _, events := read(t, b, rec); !slices.Equal(kinds(events), []session.EventKind{session.AgentMessage, session.Report}) {
		t.Fatalf("kinds = %v", kinds(events))
	}
}

func removeLeavesNothing(t *testing.T, b Backend) {
	r := recorder(t, b, session.DefaultPolicy(0), start())
	gone := begin(t, r, "refunds", session.KindRun)
	record(t, gone, session.Report, "x")
	check(t, gone.Flush(ctx))
	kept := begin(t, r, "chargebacks", session.KindRun)
	check(t, b.PutNote(ctx, session.Note{Tenant: "t1", Agent: "refunds", Name: "a", Text: "x"}))
	check(t, b.PutNote(ctx, session.Note{Tenant: "t1", Agent: "chargebacks", Name: "a", Text: "y"}))

	check(t, session.Remove(ctx, b, b, "t1", "refunds"))
	if _, _, err := b.Read(ctx, "t1", gone.Session().ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("removed session read: err = %v", err)
	}
	if l := list(t, b, "t1", 0); len(l) != 0 {
		t.Fatalf("removed agent still lists %d sessions", len(l))
	}
	if n := listNotes(t, b, "t1", "refunds"); len(n) != 0 {
		t.Fatalf("removed agent still has %d notes", len(n))
	}
	if _, _, err := b.Read(ctx, "t1", kept.Session().ID); err != nil {
		t.Fatalf("another agent's session went too: %v", err)
	}
	if n := listNotes(t, b, "t1", "chargebacks"); len(n) != 1 {
		t.Fatalf("another agent's notes went too")
	}
}

func tenantsAreApart(t *testing.T, b Backend) {
	rec := begin(t, recorder(t, b, session.DefaultPolicy(0), start()), "refunds", session.KindRun)
	id := rec.Session().ID
	if _, _, err := b.Read(ctx, "t2", id); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("t2 read t1's session: err = %v", err)
	}
	if err := b.Append(ctx, "t2", id, []session.Event{{Kind: session.AgentMessage}}); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("t2 wrote to t1's session: err = %v", err)
	}
	if err := b.End(ctx, "t2", id, session.Ended); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("t2 ended t1's session: err = %v", err)
	}
	if err := b.Delete(ctx, "t2", id); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("t2 deleted t1's session: err = %v", err)
	}
	check(t, b.DeleteAgent(ctx, "t2", "refunds"))
	if l := list(t, b, "t2", 0); len(l) != 0 {
		t.Fatalf("t2 lists t1's sessions")
	}
	if s, events := read(t, b, rec); s.Status != session.Running || len(events) != 0 {
		t.Fatalf("t1's session changed after t2's calls: %+v, %d events", s, len(events))
	}
}

func listNewestFirst(t *testing.T, b Backend) {
	c := start()
	r := recorder(t, b, session.DefaultPolicy(0), c)
	first := begin(t, r, "refunds", session.KindRun)
	c.add(time.Minute)
	second := begin(t, r, "refunds", session.KindChat)
	c.add(time.Minute)
	record(t, first, session.Report, "x") // first is now the latest updated
	check(t, first.Flush(ctx))
	l := list(t, b, "t1", 0)
	if len(l) != 2 || l[0].ID != first.Session().ID || l[1].ID != second.Session().ID {
		t.Fatalf("list = %+v", l)
	}
	if l := list(t, b, "t1", 1); len(l) != 1 || l[0].ID != first.Session().ID {
		t.Fatalf("limit 1: %+v", l)
	}
	check(t, b.Delete(ctx, "t1", first.Session().ID))
	if l := list(t, b, "t1", 0); len(l) != 1 || l[0].ID != second.Session().ID {
		t.Fatalf("after delete: %+v", l)
	}
}

// Tool calls may run in parallel and record at once: every event lands, once,
// numbered without gaps.
func concurrentRecordsAllLand(t *testing.T, b Backend) {
	rec := begin(t, recorder(t, b, session.DefaultPolicy(0), start()), "refunds", session.KindRun)
	const workers, each = 8, 50
	done := make(chan error, workers)
	for w := range workers {
		go func() {
			for i := range each {
				if err := rec.Record(session.ToolCall, []int{w, i}); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
	}
	for range workers {
		check(t, <-done)
	}
	_, events := read(t, b, rec)
	if len(events) != workers*each {
		t.Fatalf("%d events, want %d", len(events), workers*each)
	}
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d", i, e.Seq)
		}
	}
}

func notes(t *testing.T, b Backend) {
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
