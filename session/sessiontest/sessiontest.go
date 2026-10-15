// Package sessiontest checks that a session.Store and session.Notes keep the
// promises the session package makes. Every backend runs the same checks:
//
//	func TestStore(t *testing.T) {
//		sessiontest.Run(t, func(t *testing.T, now func() time.Time) sessiontest.Backend { return newBackend(t, now) })
//	}
//
// newBackend gets each check's own clock, which is the store's clock for what
// has expired, and its own t, to clean up after the check.
package sessiontest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/session"
)

// Backend is what a store under test provides.
type Backend interface {
	session.Store
	session.Notes
}

// Run runs every check, each against a fresh backend from newBackend.
func Run(t *testing.T, newBackend func(t *testing.T, now func() time.Time) Backend) {
	checks := []struct {
		name string
		fn   func(*testing.T, Backend, *clock)
	}{
		{"RecordAndRead", recordAndRead},
		{"NeverIsNotWritten", neverIsNotWritten},
		{"UntilEndIsDroppedAtEnd", untilEndIsDroppedAtEnd},
		{"EndedTakesNoEvents", endedTakesNoEvents},
		{"PurgeRemovesExpired", purgeRemovesExpired},
		{"SessionExpiresAsAWhole", sessionExpiresAsAWhole},
		{"RemoveLeavesNothing", removeLeavesNothing},
		{"TenantsAreApart", tenantsAreApart},
		{"ListNewestFirst", listNewestFirst},
		{"ConcurrentRecordsAllLand", concurrentRecordsAllLand},
		{"BodiesAreCopied", bodiesAreCopied},
		{"RetriedAppendStoresOnce", retriedAppendStoresOnce},
		{"Notes", notes},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			clock := start()
			c.fn(t, newBackend(t, clock.now), clock)
		})
	}
}

var ctx = context.Background()

// clock is a time that moves only when told. Stores read it from the
// recording's writer goroutine, so it locks.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }
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

func recordAndRead(t *testing.T, b Backend, c *clock) {
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

func neverIsNotWritten(t *testing.T, b Backend, c *clock) {
	p := session.DefaultPolicy(0)
	p[session.ToolResult] = session.Rule{Keep: session.Never}
	rec := begin(t, recorder(t, b, p, c), "refunds", session.KindRun)
	record(t, rec, session.ToolResult, map[string]string{"customer": "someone"})
	if _, events := read(t, b, rec); len(events) != 0 {
		t.Fatalf("a Never event was stored: %v", kinds(events))
	}
}

func untilEndIsDroppedAtEnd(t *testing.T, b Backend, c *clock) {
	rec := begin(t, recorder(t, b, session.DefaultPolicy(0), c), "refunds", session.KindRun)
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

func endedTakesNoEvents(t *testing.T, b Backend, c *clock) {
	rec := begin(t, recorder(t, b, session.DefaultPolicy(0), c), "refunds", session.KindRun)
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

func purgeRemovesExpired(t *testing.T, b Backend, c *clock) {
	p := session.DefaultPolicy(30 * 24 * time.Hour)
	p[session.Report] = session.Rule{Keep: session.Kept} // no retention: the session lives until deleted
	rec := begin(t, recorder(t, b, p, c), "refunds", session.KindRun)
	record(t, rec, session.OwnerMessage, "old")
	c.add(20 * 24 * time.Hour)
	record(t, rec, session.AgentMessage, "newer")
	record(t, rec, session.Report, "report")
	read(t, b, rec)

	purge(t, b, c.now())
	if _, events := read(t, b, rec); len(events) != 3 {
		t.Fatalf("%d events before anything expired, want 3", len(events))
	}
	c.add(15 * 24 * time.Hour) // the first event is 35 days old, the others 15
	if _, events := read(t, b, rec); !slices.Equal(kinds(events), []session.EventKind{session.AgentMessage, session.Report}) {
		t.Fatalf("once expired, before purge: kinds = %v", kinds(events))
	}
	purge(t, b, c.now())
	if _, events := read(t, b, rec); !slices.Equal(kinds(events), []session.EventKind{session.AgentMessage, session.Report}) {
		t.Fatalf("after purge: kinds = %v", kinds(events))
	}
}

// A session whose every kept kind has a retention goes as a whole once its last
// event has: nothing is left behind as an empty shell.
func sessionExpiresAsAWhole(t *testing.T, b Backend, c *clock) {
	r := recorder(t, b, session.DefaultPolicy(30*24*time.Hour), c)
	done := begin(t, r, "refunds", session.KindRun)
	record(t, done, session.Report, "x")
	check(t, done.End(ctx, session.Ended))
	busy := begin(t, r, "refunds", session.KindChat)
	record(t, busy, session.OwnerMessage, "first")
	c.add(20 * 24 * time.Hour)
	record(t, busy, session.OwnerMessage, "twenty days later")
	check(t, busy.Flush(ctx))

	c.add(15 * 24 * time.Hour) // done's last event is 35 days old; busy's 15
	if _, _, err := b.Read(ctx, "t1", done.Session().ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("expired session read: err = %v, want ErrNotFound", err)
	}
	l := list(t, b, "t1", 0)
	if len(l) != 1 || l[0].ID != busy.Session().ID {
		t.Fatalf("list = %+v, want only the busy session", l)
	}
	if want := c.now().Add(15 * 24 * time.Hour); !l[0].Expires.Equal(want) {
		t.Fatalf("busy expires %v, want %v (30 days after its last event)", l[0].Expires, want)
	}
	purge(t, b, c.now())
	if l := list(t, b, "t1", 0); len(l) != 1 {
		t.Fatalf("after purge, list = %+v", l)
	}
}

func removeLeavesNothing(t *testing.T, b Backend, c *clock) {
	r := recorder(t, b, session.DefaultPolicy(0), c)
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

func tenantsAreApart(t *testing.T, b Backend, c *clock) {
	rec := begin(t, recorder(t, b, session.DefaultPolicy(0), c), "refunds", session.KindRun)
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

func listNewestFirst(t *testing.T, b Backend, c *clock) {
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
func concurrentRecordsAllLand(t *testing.T, b Backend, c *clock) {
	rec := begin(t, recorder(t, b, session.DefaultPolicy(0), c), "refunds", session.KindRun)
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

func notes(t *testing.T, b Backend, c *clock) {
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

// created makes a running session straight on the store, for checks of Append.
func created(t *testing.T, b Backend) string {
	t.Helper()
	s := session.Session{ID: "s1", Tenant: "t1", Agent: "refunds", Kind: session.KindRun, Status: session.Running}
	check(t, b.Create(ctx, s))
	return s.ID
}

func event(seq int64, body string) session.Event {
	return session.Event{Seq: seq, Kind: session.Report, Body: []byte(body)}
}

func bodies(t *testing.T, b Backend, id string) []string {
	t.Helper()
	_, events, err := b.Read(ctx, "t1", id)
	check(t, err)
	var out []string
	for _, e := range events {
		out = append(out, string(e.Body))
	}
	return out
}

// What's stored can't be changed through the slices that went in or came out.
func bodiesAreCopied(t *testing.T, b Backend, c *clock) {
	id := created(t, b)
	in := []session.Event{event(1, `"stored"`)}
	check(t, b.Append(ctx, "t1", id, in))
	copy(in[0].Body, `"change"`)
	_, out, err := b.Read(ctx, "t1", id)
	check(t, err)
	copy(out[0].Body, `"change"`)
	if got := bodies(t, b, id); !slices.Equal(got, []string{`"stored"`}) {
		t.Fatalf("stored body changed from outside: %v", got)
	}
}

// An Append retried after its answer was lost stores nothing twice; a gap is
// refused.
func retriedAppendStoresOnce(t *testing.T, b Backend, c *clock) {
	id := created(t, b)
	check(t, b.Append(ctx, "t1", id, []session.Event{event(1, `1`), event(2, `2`)}))
	check(t, b.Append(ctx, "t1", id, []session.Event{event(1, `1`), event(2, `2`), event(3, `3`)}))
	if got := bodies(t, b, id); !slices.Equal(got, []string{`1`, `2`, `3`}) {
		t.Fatalf("after a retry: %v", got)
	}
	if err := b.Append(ctx, "t1", id, []session.Event{event(5, `5`)}); !errors.Is(err, session.ErrGap) {
		t.Fatalf("a gap: err = %v, want ErrGap", err)
	}
	if err := b.Append(ctx, "t1", id, []session.Event{event(4, `4`), event(6, `6`)}); !errors.Is(err, session.ErrGap) {
		t.Fatalf("a gap inside a batch: err = %v, want ErrGap", err)
	}
	if got := bodies(t, b, id); !slices.Equal(got, []string{`1`, `2`, `3`}) {
		t.Fatalf("a refused batch stored part of itself: %v", got)
	}
}
