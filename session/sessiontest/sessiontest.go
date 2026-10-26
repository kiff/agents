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
	"fmt"
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
		{"UntilEndIsDroppedWhenItsRunEnds", untilEndIsDroppedWhenItsRunEnds},
		{"EndedRunTakesNoEvents", endedRunTakesNoEvents},
		{"RunsReadInOrder", runsReadInOrder},
		{"PausedRunResumesWhereItStopped", pausedRunResumesWhereItStopped},
		{"OnlyAPausedRunResumes", onlyAPausedRunResumes},
		{"ARunThatRecordedNothingEnds", aRunThatRecordedNothingEnds},
		{"OnlyAnOpenRunTakesEvents", onlyAnOpenRunTakesEvents},
		{"HeldSessionDoesNotExpire", heldSessionDoesNotExpire},
		{"PurgeRemovesExpired", purgeRemovesExpired},
		{"SessionExpiresAsAWhole", sessionExpiresAsAWhole},
		{"RemoveLeavesNothing", removeLeavesNothing},
		{"TenantsAreApart", tenantsAreApart},
		{"ListNewestFirst", listNewestFirst},
		{"ListLimitIsTheNewest", listLimitIsTheNewest},
		{"ListOwnedIsTheOwners", listOwnedIsTheOwners},
		{"ArchivedIsKept", archivedIsKept},
		{"ConcurrentRecordsAllLand", concurrentRecordsAllLand},
		{"ConcurrentRunsInOneSession", concurrentRunsInOneSession},
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

// clock is a time that moves only when told. It starts at the real time: a
// database that deletes expired items by itself (DynamoDB Local does, within
// seconds) must never see a TTL already past. Stores read it from the
// recording's writer goroutine, so it locks.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }
func start() *clock                  { return &clock{t: time.Now().UTC().Truncate(time.Second)} }

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

const day = 24 * time.Hour

func recorder(t *testing.T, b Backend, p session.Policy, c *clock) *session.Recorder {
	t.Helper()
	r, err := session.NewRecorderAt(b, p, c.now)
	check(t, err)
	return r
}

func open(t *testing.T, r *session.Recorder, agent string, kind session.Kind) session.Session {
	t.Helper()
	s, err := r.Open(ctx, "t1", agent, kind)
	check(t, err)
	return s
}

// startRun starts recording run in s; it is ended when the test ends, if it
// wasn't.
func startRun(t *testing.T, c *clock, r *session.Recorder, s session.Session, run string) *session.Recording {
	t.Helper()
	rec, err := r.Start(ctx, s, run, c.now())
	check(t, err)
	t.Cleanup(func() { _ = rec.End(ctx) })
	return rec
}

// begin opens a session and starts recording its first run, "r1".
func begin(t *testing.T, c *clock, r *session.Recorder, agent string, kind session.Kind) *session.Recording {
	t.Helper()
	return startRun(t, c, r, open(t, r, agent, kind), "r1")
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

// order lists events as run:seq.
func order(events []session.Event) []string {
	var out []string
	for _, e := range events {
		out = append(out, fmt.Sprintf("%s:%d", e.Run, e.Seq))
	}
	return out
}

func recordAndRead(t *testing.T, b Backend, c *clock) {
	rec := begin(t, c, recorder(t, b, session.DefaultPolicy(30*day), c), "refunds", session.KindChat)
	record(t, rec, session.OwnerMessage, map[string]string{"text": "refund order 12"})
	c.add(time.Second)
	record(t, rec, session.AgentMessage, map[string]string{"text": "done"})
	record(t, rec, session.Usage, session.UsageBody{Model: "m", InputTokens: 10, OutputTokens: 5, CostMicroUSD: 1200})

	got, events := read(t, b, rec)
	if got.Agent != "refunds" || got.Kind != session.KindChat || len(got.Held) != 0 {
		t.Fatalf("session = %+v", got)
	}
	if want := []session.EventKind{session.OwnerMessage, session.AgentMessage, session.Usage}; !slices.Equal(kinds(events), want) {
		t.Fatalf("kinds = %v, want %v", kinds(events), want)
	}
	for i, e := range events {
		if e.Seq != int64(i+1) || e.Session != got.ID || e.Run != "r1" {
			t.Fatalf("event %d: seq %d, session %q, run %q", i, e.Seq, e.Session, e.Run)
		}
	}
	if !got.Updated.Equal(c.now()) {
		t.Fatalf("updated = %v, want %v", got.Updated, c.now())
	}
	if cost, err := session.Cost(events); err != nil || cost != 1200 {
		t.Fatalf("cost = %d, err = %v", cost, err)
	}
	state, own, err := b.ReadRun(ctx, "t1", got.ID, "r1")
	check(t, err)
	if state != (session.RunState{Seq: 3, Open: true}) || len(own) != 3 {
		t.Fatalf("run: %+v, %d events", state, len(own))
	}
}

func neverIsNotWritten(t *testing.T, b Backend, c *clock) {
	p := session.DefaultPolicy(0)
	p[session.ToolCall] = session.Rule{Keep: session.Never}
	p[session.ToolResult] = session.Rule{Keep: session.Never}
	rec := begin(t, c, recorder(t, b, p, c), "refunds", session.KindRun)
	record(t, rec, session.ToolResult, map[string]string{"customer": "someone"})
	if _, events := read(t, b, rec); len(events) != 0 {
		t.Fatalf("a Never event was stored: %v", kinds(events))
	}
}

// "Until the end" is the end of the event's own run: ending one run drops
// what it kept until then, and leaves another run's alone.
func untilEndIsDroppedWhenItsRunEnds(t *testing.T, b Backend, c *clock) {
	r := recorder(t, b, session.DefaultPolicy(0), c)
	s := open(t, r, "refunds", session.KindChat)
	first, second := startRun(t, c, r, s, "r1"), startRun(t, c, r, s, "r2")
	for _, rec := range []*session.Recording{first, second} {
		record(t, rec, session.ToolCall, map[string]string{"tool": "list_charges"})
		record(t, rec, session.ToolResult, map[string]string{"charges": "…"})
		record(t, rec, session.AgentMessage, map[string]string{"text": "refunded 1"})
		check(t, rec.Flush(ctx))
	}
	if _, during := read(t, b, first); len(during) != 6 {
		t.Fatalf("during the runs, %d events, want 6: the agent still needs the results", len(during))
	}
	check(t, first.End(ctx))
	_, after := read(t, b, second)
	if want := []string{"r1:3", "r2:1", "r2:2", "r2:3"}; !slices.Equal(order(after), want) {
		t.Fatalf("after r1 ends: %v, want %v", order(after), want)
	}
	if _, own, err := b.ReadRun(ctx, "t1", s.ID, "r2"); err != nil || !slices.Equal(order(own), []string{"r2:1", "r2:2", "r2:3"}) {
		t.Fatalf("r2's own events: %v, err = %v", order(own), err)
	}
	check(t, second.End(ctx))
	_, after = read(t, b, second)
	if want := []string{"r1:3", "r2:3"}; !slices.Equal(order(after), want) {
		t.Fatalf("after both end: %v, want %v", order(after), want)
	}
}

func endedRunTakesNoEvents(t *testing.T, b Backend, c *clock) {
	r := recorder(t, b, session.DefaultPolicy(0), c)
	rec := begin(t, c, r, "refunds", session.KindChat)
	id := rec.Session().ID
	check(t, rec.End(ctx))
	if err := rec.Record(session.AgentMessage, "late"); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("recording after the end: err = %v, want ErrEnded", err)
	}
	if err := rec.End(ctx); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("ending twice: err = %v, want ErrEnded", err)
	}
	if err := b.Append(ctx, "t1", id, "r1", []session.Event{{Seq: 1, Kind: session.AgentMessage}}); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("appending to an ended run: err = %v, want ErrEnded", err)
	}
	if err := b.EndRun(ctx, "t1", id, "r1", time.Time{}); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("ending an ended run on the store: err = %v, want ErrEnded", err)
	}
	if err := b.Hold(ctx, "t1", id, "r1"); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("holding an ended run: err = %v, want ErrEnded", err)
	}
	if _, err := r.Resume(ctx, rec.Session(), "r1"); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("resuming an ended run: err = %v, want ErrEnded", err)
	}
	if state, err := b.State(ctx, "t1", id, "r1"); err != nil || !state.Ended {
		t.Fatalf("run state = %+v, err = %v", state, err)
	}
	next := startRun(t, c, r, rec.Session(), "r2") // the session goes on
	record(t, next, session.OwnerMessage, "next")
	if _, events := read(t, b, next); !slices.Equal(order(events), []string{"r2:1"}) {
		t.Fatalf("events = %v", order(events))
	}
}

// A session's runs read one after the other, in the order they started, even
// when they recorded at the same time.
func runsReadInOrder(t *testing.T, b Backend, c *clock) {
	r := recorder(t, b, session.DefaultPolicy(0), c)
	s := open(t, r, "refunds", session.KindChat)
	later, earlier := startRun(t, c, r, s, "a-later"), startRun(t, c, r, s, "z-earlier") // run IDs don't decide
	record(t, earlier, session.OwnerMessage, "1")
	check(t, earlier.Flush(ctx))
	c.add(time.Second)
	record(t, later, session.OwnerMessage, "2")
	check(t, later.Flush(ctx))
	c.add(time.Second)
	record(t, earlier, session.AgentMessage, "3")
	record(t, later, session.AgentMessage, "4")
	check(t, earlier.Flush(ctx))
	_, events := read(t, b, later)
	if want := []string{"z-earlier:1", "z-earlier:2", "a-later:1", "a-later:2"}; !slices.Equal(order(events), want) {
		t.Fatalf("order = %v, want %v", order(events), want)
	}
}

// A paused run carries on in a fresh recording, as in a fresh process: its
// events number on from where it stopped, and what it kept until its end is
// still there until it ends.
func pausedRunResumesWhereItStopped(t *testing.T, b Backend, c *clock) {
	r := recorder(t, b, session.DefaultPolicy(30*day), c)
	rec := begin(t, c, r, "refunds", session.KindRun)
	record(t, rec, session.OwnerMessage, "refund order 12")
	record(t, rec, session.ToolCall, "refund")
	record(t, rec, session.ToolResult, "held")
	check(t, rec.Pause(ctx))
	if err := rec.Record(session.AgentMessage, "late"); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("recording after a pause: err = %v, want ErrEnded", err)
	}
	if err := rec.Pause(ctx); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("pausing twice: err = %v, want ErrEnded", err)
	}
	id := rec.Session().ID
	if state, _, err := b.ReadRun(ctx, "t1", id, "r1"); err != nil || state != (session.RunState{Seq: 3, Open: true, Held: true}) {
		t.Fatalf("paused run state = %+v, err = %v", state, err)
	}
	if state, err := b.State(ctx, "t1", id, "r1"); err != nil || state != (session.RunState{Seq: 3, Open: true, Held: true}) {
		t.Fatalf("paused run State = %+v, err = %v", state, err)
	}
	check(t, b.Hold(ctx, "t1", id, "r1")) // holding again changes nothing

	fresh := recorder(t, b, session.DefaultPolicy(30*day), c)
	s, _, err := b.Read(ctx, "t1", id)
	check(t, err)
	if !slices.Equal(s.Held, []string{"r1"}) {
		t.Fatalf("held = %v", s.Held)
	}
	more, err := fresh.Resume(ctx, s, "r1")
	check(t, err)
	t.Cleanup(func() { _ = more.End(ctx) })
	record(t, more, session.ToolResult, "approved")
	record(t, more, session.Report, "refunded")
	check(t, more.End(ctx))
	state, events, err := b.ReadRun(ctx, "t1", id, "r1")
	check(t, err)
	if state != (session.RunState{Seq: 5, Ended: true}) {
		t.Fatalf("ended run state = %+v", state)
	}
	if want := []string{"r1:1", "r1:5"}; !slices.Equal(order(events), want) {
		t.Fatalf("events = %v, want %v: the tool call and results go when the run ends", order(events), want)
	}
	if s, _, _ := b.Read(ctx, "t1", id); len(s.Held) != 0 {
		t.Fatalf("held after the end = %v", s.Held)
	}
}

// A run still recording has its writer: a second Recording would number from
// the same Seq, and the Store would skip its events as retries. So only a held
// run resumes.
func onlyAPausedRunResumes(t *testing.T, b Backend, c *clock) {
	r := recorder(t, b, session.DefaultPolicy(0), c)
	rec := begin(t, c, r, "refunds", session.KindRun)
	record(t, rec, session.AgentMessage, "1")
	check(t, rec.Flush(ctx))
	if _, err := r.Resume(ctx, rec.Session(), "r1"); !errors.Is(err, session.ErrNotPaused) {
		t.Fatalf("resuming a live run: err = %v, want ErrNotPaused", err)
	}
	if _, err := r.Resume(ctx, rec.Session(), "never"); !errors.Is(err, session.ErrNotPaused) {
		t.Fatalf("resuming a run never recorded: err = %v, want ErrNotPaused", err)
	}
	if state, err := b.State(ctx, "t1", rec.Session().ID, "never"); err != nil || state != (session.RunState{}) {
		t.Fatalf("a run never recorded: %+v, err = %v", state, err)
	}
	if _, err := b.State(ctx, "t2", rec.Session().ID, "r1"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("t2 read t1's run state: err = %v", err)
	}
}

// A run can end with nothing recorded (every kind Never, or a failure before
// its first event); once ended, it takes nothing more, and never opens again.
func aRunThatRecordedNothingEnds(t *testing.T, b Backend, c *clock) {
	id := created(t, b, c)
	check(t, b.Begin(ctx, "t1", id, "r1", c.now())) // beginning an open run again changes nothing
	check(t, b.EndRun(ctx, "t1", id, "r1", time.Time{}))
	if err := b.Begin(ctx, "t1", id, "r1", c.now()); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("beginning an ended run: err = %v, want ErrEnded", err)
	}
	if state, err := b.State(ctx, "t1", id, "r1"); err != nil || state != (session.RunState{Ended: true}) {
		t.Fatalf("state = %+v, err = %v", state, err)
	}
	if err := b.Append(ctx, "t1", id, "r1", []session.Event{event(1, `1`)}); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("append after the end: err = %v, want ErrEnded", err)
	}
	if err := b.EndRun(ctx, "t1", id, "r1", time.Time{}); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("ending again: err = %v, want ErrEnded", err)
	}
	if err := b.Hold(ctx, "t1", id, "r1"); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("holding after the end: err = %v, want ErrEnded", err)
	}
	if got := bodies(t, b, id); len(got) != 0 {
		t.Fatalf("events = %v", got)
	}
}

// Only a begun run takes events or a hold; a run created more than
// MaxStartDelay ago can't begin.
func onlyAnOpenRunTakesEvents(t *testing.T, b Backend, c *clock) {
	id := created(t, b, c)
	if err := b.Append(ctx, "t1", id, "never", []session.Event{event(1, `1`)}); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("append to a run never begun: err = %v, want ErrEnded", err)
	}
	if err := b.Hold(ctx, "t1", id, "never"); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("hold of a run never begun: err = %v, want ErrEnded", err)
	}
	if err := b.EndRun(ctx, "t1", id, "never", time.Time{}); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("end of a run never begun: err = %v, want ErrEnded", err)
	}
	if err := b.Begin(ctx, "t1", id, "old", c.now().Add(-session.MaxStartDelay-time.Second)); !errors.Is(err, session.ErrLate) {
		t.Fatalf("begin of a run created too long ago: err = %v, want ErrLate", err)
	}
	check(t, b.Begin(ctx, "t1", id, "recent", c.now().Add(-session.MaxStartDelay+time.Second)))
	if state, err := b.State(ctx, "t1", id, "recent"); err != nil || state != (session.RunState{Open: true}) {
		t.Fatalf("a begun run: %+v, err = %v", state, err)
	}
	if got := bodies(t, b, id); len(got) != 0 {
		t.Fatalf("events = %v", got)
	}
}

// A paused run keeps its session past its Expires; once the run ends,
// retention starts again from then.
func heldSessionDoesNotExpire(t *testing.T, b Backend, c *clock) {
	r := recorder(t, b, session.DefaultPolicy(day), c)
	rec := begin(t, c, r, "refunds", session.KindRun)
	record(t, rec, session.OwnerMessage, "refund order 12")
	check(t, rec.Pause(ctx))
	id := rec.Session().ID

	c.add(5 * day) // well past the session's Expires, and the message's
	if l := list(t, b, "t1", 0); len(l) != 1 {
		t.Fatalf("a held session isn't listed: %+v", l)
	}
	purge(t, b, c.now())
	s, _, err := b.Read(ctx, "t1", id)
	if err != nil {
		t.Fatalf("a held session expired: %v", err)
	}
	more, err := r.Resume(ctx, s, "r1")
	check(t, err)
	check(t, more.End(ctx))
	s, _, err = b.Read(ctx, "t1", id)
	check(t, err)
	if want := c.now().Add(day); !s.Expires.Equal(want) {
		t.Fatalf("expires %v, want %v: a day after the run ended", s.Expires, want)
	}
	c.add(day + time.Second)
	if _, _, err := b.Read(ctx, "t1", id); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("a day after its run ended: err = %v, want ErrNotFound", err)
	}
	if l := list(t, b, "t1", 0); len(l) != 0 {
		t.Fatalf("expired session listed: %+v", l)
	}
}

func purgeRemovesExpired(t *testing.T, b Backend, c *clock) {
	p := session.DefaultPolicy(30 * day)
	p[session.Report] = session.Rule{Keep: session.Kept} // no retention: the session lives until deleted
	rec := begin(t, c, recorder(t, b, p, c), "refunds", session.KindRun)
	record(t, rec, session.OwnerMessage, "old")
	c.add(20 * day)
	record(t, rec, session.AgentMessage, "newer")
	record(t, rec, session.Report, "report")
	read(t, b, rec)

	purge(t, b, c.now())
	if _, events := read(t, b, rec); len(events) != 3 {
		t.Fatalf("%d events before anything expired, want 3", len(events))
	}
	c.add(15 * day) // the first event is 35 days old, the others 15
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
	r := recorder(t, b, session.DefaultPolicy(30*day), c)
	done := begin(t, c, r, "refunds", session.KindRun)
	record(t, done, session.Report, "x")
	check(t, done.End(ctx))
	busy := begin(t, c, r, "refunds", session.KindChat)
	record(t, busy, session.OwnerMessage, "first")
	c.add(20 * day)
	record(t, busy, session.OwnerMessage, "twenty days later")
	check(t, busy.Flush(ctx))

	c.add(15 * day) // done's last event is 35 days old; busy's 15
	if _, _, err := b.Read(ctx, "t1", done.Session().ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("expired session read: err = %v, want ErrNotFound", err)
	}
	if _, _, err := b.ReadRun(ctx, "t1", done.Session().ID, "r1"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("expired session's run read: err = %v, want ErrNotFound", err)
	}
	l := list(t, b, "t1", 0)
	if len(l) != 1 || l[0].ID != busy.Session().ID {
		t.Fatalf("list = %+v, want only the busy session", l)
	}
	if want := c.now().Add(15 * day); !l[0].Expires.Equal(want) {
		t.Fatalf("busy expires %v, want %v (30 days after its last event)", l[0].Expires, want)
	}
	purge(t, b, c.now())
	if l := list(t, b, "t1", 0); len(l) != 1 {
		t.Fatalf("after purge, list = %+v", l)
	}
}

func removeLeavesNothing(t *testing.T, b Backend, c *clock) {
	r := recorder(t, b, session.DefaultPolicy(0), c)
	gone := begin(t, c, r, "refunds", session.KindRun)
	record(t, gone, session.Report, "x")
	check(t, gone.Pause(ctx)) // a held run doesn't keep its session from removal
	kept := begin(t, c, r, "chargebacks", session.KindRun)
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
	rec := begin(t, c, recorder(t, b, session.DefaultPolicy(0), c), "refunds", session.KindRun)
	id := rec.Session().ID
	if _, _, err := b.Read(ctx, "t2", id); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("t2 read t1's session: err = %v", err)
	}
	if _, _, err := b.ReadRun(ctx, "t2", id, "r1"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("t2 read t1's run: err = %v", err)
	}
	if err := b.Begin(ctx, "t2", id, "r9", c.now()); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("t2 began a run in t1's session: err = %v", err)
	}
	if err := b.Append(ctx, "t2", id, "r1", []session.Event{{Seq: 1, Kind: session.AgentMessage}}); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("t2 wrote to t1's session: err = %v", err)
	}
	if err := b.Hold(ctx, "t2", id, "r1"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("t2 held t1's run: err = %v", err)
	}
	if err := b.EndRun(ctx, "t2", id, "r1", time.Time{}); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("t2 ended t1's run: err = %v", err)
	}
	if err := b.Delete(ctx, "t2", id); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("t2 deleted t1's session: err = %v", err)
	}
	check(t, b.DeleteAgent(ctx, "t2", "refunds"))
	if l := list(t, b, "t2", 0); len(l) != 0 {
		t.Fatalf("t2 lists t1's sessions")
	}
	s, events := read(t, b, rec)
	state, _, err := b.ReadRun(ctx, "t1", id, "r1")
	check(t, err)
	if len(s.Held) != 0 || len(events) != 0 || state != (session.RunState{Open: true}) {
		t.Fatalf("t1's session changed after t2's calls: %+v, %d events, run %+v", s, len(events), state)
	}
}

func listNewestFirst(t *testing.T, b Backend, c *clock) {
	r := recorder(t, b, session.DefaultPolicy(0), c)
	first := begin(t, c, r, "refunds", session.KindRun)
	c.add(time.Minute)
	second := begin(t, c, r, "refunds", session.KindChat)
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

// Whatever its limit, List returns the newest of the whole list: sessions
// updated close together, or some again much later, are ordered the same.
func listLimitIsTheNewest(t *testing.T, b Backend, c *clock) {
	r := recorder(t, b, session.DefaultPolicy(0), c)
	var recs []*session.Recording
	for range 12 {
		recs = append(recs, begin(t, c, r, "refunds", session.KindChat))
		c.add(25 * time.Second) // a few to a minute
	}
	for _, i := range []int{2, 7, 3} {
		c.add(40 * time.Second)
		record(t, recs[i], session.OwnerMessage, "again")
		check(t, recs[i].Flush(ctx))
	}
	all := list(t, b, "t1", 0)
	if len(all) != 12 {
		t.Fatalf("list = %d sessions, want 12", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].Updated.After(all[i-1].Updated) {
			t.Fatalf("list isn't newest first at %d: %v after %v", i, all[i].Updated, all[i-1].Updated)
		}
	}
	if all[0].ID != recs[3].Session().ID || all[1].ID != recs[7].Session().ID || all[2].ID != recs[2].Session().ID {
		t.Fatalf("the sessions updated last don't come first")
	}
	for limit := 1; limit <= 12; limit++ {
		l := list(t, b, "t1", limit)
		if len(l) != limit {
			t.Fatalf("limit %d: %d sessions", limit, len(l))
		}
		for i := range l {
			if l[i].ID != all[i].ID {
				t.Fatalf("limit %d: session %d is %s, want %s", limit, i, l[i].ID, all[i].ID)
			}
		}
	}
}

// An owned session is its owner's thread (#219): ListOwned gives an owner
// their sessions and the ones from before owners, never another owner's,
// newest first, after the session moves in the index as it's written to.
func listOwnedIsTheOwners(t *testing.T, b Backend, c *clock) {
	r := recorder(t, b, session.DefaultPolicy(0), c)
	openOwned := func(owner string) *session.Recording {
		t.Helper()
		s, err := r.OpenOwned(ctx, "t1", "refunds", session.KindChat, owner)
		check(t, err)
		return startRun(t, c, r, s, "r1")
	}
	legacy := begin(t, c, r, "refunds", session.KindChat)
	c.add(time.Minute)
	job := begin(t, c, r, "refunds", session.KindRun)
	c.add(time.Minute)
	unknown := begin(t, c, r, "refunds", "") // no kind recorded: no member's
	c.add(time.Minute)
	ana := openOwned("member:ana")
	c.add(time.Minute)
	luis := openOwned("member:luis")
	c.add(time.Minute)
	anaAgain := openOwned("member:ana")
	c.add(2 * time.Minute)
	record(t, ana, session.OwnerMessage, "again") // moves ana's first chat in the index
	check(t, ana.Flush(ctx))

	ids := func(l []session.Session) []string {
		var out []string
		for _, s := range l {
			out = append(out, s.ID)
		}
		return out
	}
	got, err := b.ListOwned(ctx, "t1", "refunds", "member:ana", 0)
	check(t, err)
	// A chat with no owner is no member's: it isn't listed to ana. A job's
	// session is the taf's, listed as before.
	want := []string{ana.Session().ID, anaAgain.Session().ID, job.Session().ID}
	if !slices.Equal(ids(got), want) {
		t.Fatalf("ana's = %v, want %v (not %s or %s, which have no owner)", ids(got), want, legacy.Session().ID, unknown.Session().ID)
	}
	if got[0].Owner != "member:ana" {
		t.Fatalf("owner = %q", got[0].Owner)
	}
	if got, err := b.ListOwned(ctx, "t1", "refunds", "member:ana", 1); err != nil || !slices.Equal(ids(got), want[:1]) {
		t.Fatalf("ana's, limit 1 = %v, %v", ids(got), err)
	}
	got, err = b.ListOwned(ctx, "t1", "refunds", "member:luis", 0)
	check(t, err)
	if want := []string{luis.Session().ID, job.Session().ID}; !slices.Equal(ids(got), want) {
		t.Fatalf("luis's = %v, want %v", ids(got), want)
	}
	if s, _ := read(t, b, luis); s.Owner != "member:luis" {
		t.Fatalf("read back, owner = %q", s.Owner)
	}
	if all := list(t, b, "t1", 0); len(all) != 6 {
		t.Fatalf("List still lists every session: %d", len(all))
	}
}

// A chat its owner archives says so when listed and read, and can be put
// back; one that isn't there can't be archived.
func archivedIsKept(t *testing.T, b Backend, c *clock) {
	r := recorder(t, b, session.DefaultPolicy(0), c)
	s, err := r.OpenOwned(ctx, "t1", "refunds", session.KindChat, "member:ana")
	check(t, err)
	check(t, b.SetArchived(ctx, "t1", s.ID, true))
	got, err := b.ListOwned(ctx, "t1", "refunds", "member:ana", 0)
	check(t, err)
	if len(got) != 1 || !got[0].Archived || got[0].Owner != "member:ana" {
		t.Fatalf("listed = %+v", got)
	}
	if read, _, err := b.Read(ctx, "t1", s.ID); err != nil || !read.Archived {
		t.Fatalf("read = %+v, %v", read, err)
	}
	check(t, b.SetArchived(ctx, "t1", s.ID, false))
	if read, _, _ := b.Read(ctx, "t1", s.ID); read.Archived {
		t.Fatal("put back, it's still archived")
	}
	if err := b.SetArchived(ctx, "t1", "nope", true); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("archiving a session that isn't there: %v", err)
	}
	if err := b.SetArchived(ctx, "t2", s.ID, true); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("archiving another tenant's session: %v", err)
	}
}

// Tool calls may run in parallel and record at once: every event lands, once,
// numbered without gaps.
func concurrentRecordsAllLand(t *testing.T, b Backend, c *clock) {
	rec := begin(t, c, recorder(t, b, session.DefaultPolicy(0), c), "refunds", session.KindRun)
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

// Runs in one session record at once (a paused run continuing while the owner
// sends a new message): each numbers its own events, and all of them land.
func concurrentRunsInOneSession(t *testing.T, b Backend, c *clock) {
	r := recorder(t, b, session.DefaultPolicy(0), c)
	s := open(t, r, "refunds", session.KindChat)
	const runs, each = 4, 30
	recs := make([]*session.Recording, runs)
	for i := range recs {
		recs[i] = startRun(t, c, r, s, fmt.Sprintf("r%d", i))
	}
	done := make(chan error, runs)
	for _, rec := range recs {
		go func() {
			for i := range each {
				if err := rec.Record(session.AgentMessage, i); err != nil {
					done <- err
					return
				}
				if i%10 == 9 { // one write at a time per run, so the runs' writes cross
					if err := rec.Flush(ctx); err != nil {
						done <- err
						return
					}
				}
			}
			done <- rec.End(ctx)
		}()
	}
	for range runs {
		check(t, <-done)
	}
	_, events, err := b.Read(ctx, "t1", s.ID)
	check(t, err)
	if len(events) != runs*each {
		t.Fatalf("%d events, want %d", len(events), runs*each)
	}
	seen := map[string]int64{}
	for _, e := range events {
		if e.Seq != seen[e.Run]+1 {
			t.Fatalf("run %s: seq %d after %d", e.Run, e.Seq, seen[e.Run])
		}
		seen[e.Run] = e.Seq
	}
}

func notes(t *testing.T, b Backend, c *clock) {
	put := func(name, text string) {
		check(t, b.PutNote(ctx, session.Note{Tenant: "t1", Agent: "refunds", Name: name, Text: text, Run: "r0"}))
	}
	put("b", "1")
	put("a", "2")
	put("b", "3")
	n := listNotes(t, b, "t1", "refunds")
	if len(n) != 2 || n[0].Name != "a" || n[1].Text != "3" {
		t.Fatalf("notes = %+v", n)
	}
	check(t, b.DeleteNote(ctx, "t1", "refunds", session.Shared, "a"))
	check(t, b.DeleteNote(ctx, "t1", "refunds", session.Shared, "missing"))
	if n := listNotes(t, b, "t2", "refunds"); len(n) != 0 {
		t.Fatalf("t2 sees t1's notes")
	}
	if n := listNotes(t, b, "t1", "refunds"); len(n) != 1 || n[0].Name != "b" {
		t.Fatalf("after forgetting: %+v", n)
	}

	// Scopes (#219): a requester's note of a name is apart from the shared
	// one and from another requester's, and keeps where it came from.
	ana, luis := "member:ana", "member:luis"
	check(t, b.PutNote(ctx, session.Note{Tenant: "t1", Agent: "refunds", Scope: ana, Name: "b", Text: "ana's",
		Session: "s1", Run: "r1", Requester: ana}))
	check(t, b.PutNote(ctx, session.Note{Tenant: "t1", Agent: "refunds", Scope: luis, Name: "b", Text: "luis's"}))
	// A name that looks like a scoped key stays a shared note of its own.
	check(t, b.PutNote(ctx, session.Note{Tenant: "t1", Agent: "refunds", Name: "member:ana|b", Text: "shared", SharedBy: "member:ana"}))
	n = listNotes(t, b, "t1", "refunds")
	if len(n) != 4 || n[0].Scope != session.Shared || n[0].Name != "b" || n[0].Text != "3" ||
		n[1].Name != "member:ana|b" || n[2].Scope != ana || n[2].Text != "ana's" || n[3].Scope != luis || n[3].Text != "luis's" {
		t.Fatalf("scoped notes = %+v", n)
	}
	if n[2].Session != "s1" || n[2].Run != "r1" || n[2].Requester != ana {
		t.Fatalf("ana's note lost its source: %+v", n[2])
	}
	if got := session.NotesFor(n, ana); len(got) != 3 || got[2].Text != "ana's" {
		t.Fatalf("ana reads %+v", got)
	}
	if got := session.NotesFor(n, ""); len(got) != 2 {
		t.Fatalf("no requester reads %+v", got)
	}
	check(t, b.DeleteNote(ctx, "t1", "refunds", ana, "b"))
	n = listNotes(t, b, "t1", "refunds")
	if len(n) != 3 || n[0].Text != "3" || n[2].Scope != luis {
		t.Fatalf("after ana forgets hers: %+v", n)
	}
	check(t, b.DeleteNotes(ctx, "t1", "refunds"))
	if n := listNotes(t, b, "t1", "refunds"); len(n) != 0 {
		t.Fatalf("after forgetting all: %+v", n)
	}
}

// created makes a session straight on the store, with runs r1 and r2 begun,
// for checks of Append.
func created(t *testing.T, b Backend, c *clock) string {
	t.Helper()
	s := session.Session{ID: "s1", Tenant: "t1", Agent: "refunds", Kind: session.KindRun}
	check(t, b.Create(ctx, s))
	check(t, b.Begin(ctx, "t1", s.ID, "r1", c.now()))
	check(t, b.Begin(ctx, "t1", s.ID, "r2", c.now()))
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
		out = append(out, e.Run+"="+string(e.Body))
	}
	return out
}

// What's stored can't be changed through the slices that went in or came out.
func bodiesAreCopied(t *testing.T, b Backend, c *clock) {
	id := created(t, b, c)
	in := []session.Event{event(1, `"stored"`)}
	check(t, b.Append(ctx, "t1", id, "r1", in))
	copy(in[0].Body, `"change"`)
	_, out, err := b.Read(ctx, "t1", id)
	check(t, err)
	copy(out[0].Body, `"change"`)
	if got := bodies(t, b, id); !slices.Equal(got, []string{`r1="stored"`}) {
		t.Fatalf("stored body changed from outside: %v", got)
	}
}

// An Append retried after its answer was lost stores nothing twice; a gap is
// refused. Each run has its own sequence.
func retriedAppendStoresOnce(t *testing.T, b Backend, c *clock) {
	id := created(t, b, c)
	check(t, b.Append(ctx, "t1", id, "r1", []session.Event{event(1, `1`), event(2, `2`)}))
	check(t, b.Append(ctx, "t1", id, "r1", []session.Event{event(1, `1`), event(2, `2`), event(3, `3`)}))
	if got := bodies(t, b, id); !slices.Equal(got, []string{`r1=1`, `r1=2`, `r1=3`}) {
		t.Fatalf("after a retry: %v", got)
	}
	if err := b.Append(ctx, "t1", id, "r1", []session.Event{event(5, `5`)}); !errors.Is(err, session.ErrGap) {
		t.Fatalf("a gap: err = %v, want ErrGap", err)
	}
	if err := b.Append(ctx, "t1", id, "r1", []session.Event{event(4, `4`), event(6, `6`)}); !errors.Is(err, session.ErrGap) {
		t.Fatalf("a gap inside a batch: err = %v, want ErrGap", err)
	}
	if err := b.Append(ctx, "t1", id, "r2", []session.Event{event(2, `x`)}); !errors.Is(err, session.ErrGap) {
		t.Fatalf("a new run starting at 2: err = %v, want ErrGap", err)
	}
	check(t, b.Append(ctx, "t1", id, "r2", []session.Event{event(1, `a`)}))
	if got := bodies(t, b, id); !slices.Equal(got, []string{`r1=1`, `r1=2`, `r1=3`, `r2=a`}) {
		t.Fatalf("a refused batch stored part of itself: %v", got)
	}
}
