package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
)

var ctx = context.Background()

func TestPolicyNeedsEveryKind(t *testing.T) {
	p := session.DefaultPolicy(time.Hour)
	if err := p.Check(); err != nil {
		t.Fatal(err)
	}
	delete(p, session.Usage)
	if _, err := session.NewRecorder(memory.New(), p); err == nil {
		t.Fatal("a policy without a rule for usage was accepted")
	}
}

func TestPolicyRefusesRetentionOnUnkept(t *testing.T) {
	p := session.DefaultPolicy(0)
	p[session.Report] = session.Rule{Keep: session.UntilEnd, Retention: time.Hour}
	if err := p.Check(); err == nil {
		t.Fatal("retention on an until-end kind was accepted")
	}
	p[session.Report] = session.Rule{Keep: session.Kept, Retention: -time.Hour}
	if err := p.Check(); err == nil {
		t.Fatal("negative retention was accepted")
	}
}

// What is kept of a run never holds a tool call without its result, or a
// result without its call.
func TestPolicyKeepsAToolCallWithItsResult(t *testing.T) {
	p := session.DefaultPolicy(time.Hour)
	p[session.ToolCall] = session.Rule{Keep: session.Kept, Retention: time.Hour}
	if err := p.Check(); err == nil {
		t.Fatal("a policy keeping tool calls longer than their results was accepted")
	}
	p[session.ToolResult] = session.Rule{Keep: session.Kept, Retention: 2 * time.Hour}
	if err := p.Check(); err == nil {
		t.Fatal("a policy keeping tool calls and results for different retentions was accepted")
	}
	p[session.ToolResult] = p[session.ToolCall]
	if err := p.Check(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionRetention(t *testing.T) {
	p := session.DefaultPolicy(30 * 24 * time.Hour)
	if got := p.SessionRetention(); got != 30*24*time.Hour {
		t.Fatalf("default: %v", got)
	}
	p[session.Report] = session.Rule{Keep: session.Kept, Retention: 90 * 24 * time.Hour}
	if got := p.SessionRetention(); got != 90*24*time.Hour {
		t.Fatalf("the longest kept kind decides: %v", got)
	}
	p[session.Usage] = session.Rule{Keep: session.Kept}
	if got := p.SessionRetention(); got != 0 {
		t.Fatalf("a kind kept with no retention keeps the session: %v", got)
	}
}

// A body over MaxBody is recorded as a placeholder that says how big it was:
// the run goes on, and the store never sees an event it can't keep.
func TestABodyOverMaxBodyIsOmitted(t *testing.T) {
	f := newFlaky()
	rec := start(t, f)
	big := strings.Repeat("a", session.MaxBody)
	if err := rec.Record(session.ToolResult, big); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := rec.Record(session.Report, "small"); err != nil {
		t.Fatal(err)
	}
	if err := rec.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	_, events := stored(t, f, rec)
	var got session.OmittedBody
	if len(events) != 2 || events[0].Kind != session.ToolResult || json.Unmarshal(events[0].Body, &got) != nil {
		t.Fatalf("events = %+v", events)
	}
	if got.Omitted != "over MaxBody" || got.Bytes != session.MaxBody+2 { // the JSON string's quotes
		t.Fatalf("placeholder = %+v", got)
	}
	if err := rec.End(ctx); err != nil {
		t.Fatal(err)
	}
}

// Batches stay under MaxBatchBytes, so a store's transaction limit holds.
func TestBatchesStayUnderMaxBatchBytes(t *testing.T) {
	f := newFlaky()
	rec := start(t, f)
	f.block()
	body := strings.Repeat("a", 200<<10) // about 200 KB encoded
	for range 20 {
		if err := rec.Record(session.AgentMessage, body); err != nil {
			t.Fatal(err)
		}
	}
	f.release()
	if err := rec.End(ctx); err != nil {
		t.Fatal(err)
	}
	if max := f.maxBatchBytes.Load(); max > session.MaxBatchBytes {
		t.Fatalf("a batch of %d bytes, over %d", max, session.MaxBatchBytes)
	}
	if _, events := stored(t, f, rec); len(events) != 20 {
		t.Fatalf("%d events stored", len(events))
	}
}

func TestDefaultPolicyKeepsToolsOnlyForTheRun(t *testing.T) {
	p := session.DefaultPolicy(30 * 24 * time.Hour)
	if p[session.ToolCall].Keep != session.UntilEnd || p[session.ToolResult].Keep != session.UntilEnd {
		t.Fatalf("tool calls: %+v, results: %+v", p[session.ToolCall], p[session.ToolResult])
	}
	if p[session.OwnerMessage] != (session.Rule{Keep: session.Kept, Retention: 30 * 24 * time.Hour}) {
		t.Fatalf("owner messages: %+v", p[session.OwnerMessage])
	}
}

// flaky is a store a test controls: appends wait at a gate the test opens, and
// appends or ends can be made to fail.
type flaky struct {
	*memory.Store
	appends       atomic.Int64
	maxBatchBytes atomic.Int64

	mu        sync.Mutex
	gate      chan struct{} // appends wait until it's closed
	fail      bool          // appends fail without storing
	loseNext  bool          // the next append stores, then fails (its answer is lost)
	endFails  int           // this many EndRuns fail
	holdFails int           // this many Holds fail
}

var errDown = errors.New("store down")

func newFlaky() *flaky {
	open := make(chan struct{})
	close(open)
	return &flaky{Store: memory.New(), gate: open}
}

func (f *flaky) block() { f.mu.Lock(); f.gate = make(chan struct{}); f.mu.Unlock() }
func (f *flaky) release() {
	f.mu.Lock()
	close(f.gate)
	f.mu.Unlock()
}
func (f *flaky) set(fn func(f *flaky)) { f.mu.Lock(); fn(f); f.mu.Unlock() }

func (f *flaky) Append(ctx context.Context, tenant, id, run string, events []session.Event) error {
	f.appends.Add(1)
	size := 0
	for _, e := range events {
		size += len(e.Body)
	}
	for {
		m := f.maxBatchBytes.Load()
		if int64(size) <= m || f.maxBatchBytes.CompareAndSwap(m, int64(size)) {
			break
		}
	}
	f.mu.Lock()
	gate := f.gate
	f.mu.Unlock()
	<-gate
	f.mu.Lock()
	fail, lose := f.fail, f.loseNext
	f.loseNext = false
	f.mu.Unlock()
	if fail {
		return errDown
	}
	if err := f.Store.Append(ctx, tenant, id, run, events); err != nil {
		return err
	}
	if lose {
		return errDown
	}
	return nil
}

func (f *flaky) EndRun(ctx context.Context, tenant, id, run string, expires time.Time) error {
	f.mu.Lock()
	failing := f.endFails > 0
	if failing {
		f.endFails--
	}
	f.mu.Unlock()
	if failing {
		return errDown
	}
	return f.Store.EndRun(ctx, tenant, id, run, expires)
}

func (f *flaky) Hold(ctx context.Context, tenant, id, run string) error {
	f.mu.Lock()
	failing := f.holdFails > 0
	if failing {
		f.holdFails--
	}
	f.mu.Unlock()
	if failing {
		return errDown
	}
	return f.Store.Hold(ctx, tenant, id, run)
}

func start(t *testing.T, f *flaky) *session.Recording {
	t.Helper()
	r, err := session.NewRecorder(f, session.DefaultPolicy(0))
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Open(ctx, "t1", "refunds", session.KindRun)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := r.Start(ctx, s, "r1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func stored(t *testing.T, f *flaky, rec *session.Recording) (session.RunState, []session.Event) {
	t.Helper()
	state, events, err := f.ReadRun(ctx, "t1", rec.Session().ID, rec.Run())
	if err != nil {
		t.Fatal(err)
	}
	return state, events
}

func deadline(t *testing.T, d time.Duration) context.Context {
	c, cancel := context.WithTimeout(ctx, d)
	t.Cleanup(cancel)
	return c
}

// The run loop doesn't wait on the store: Record returns while a write is
// stuck, and what queues meanwhile goes in the next batches.
func TestRecordDoesNotWaitAndBatches(t *testing.T) {
	f := newFlaky()
	rec := start(t, f)
	f.block()
	for i := range 50 {
		if err := rec.Record(session.AgentMessage, i); err != nil {
			t.Fatal(err)
		}
	}
	f.release()
	if err := rec.End(ctx); err != nil {
		t.Fatal(err)
	}
	if _, events := stored(t, f, rec); len(events) != 50 {
		t.Fatalf("%d events stored, want 50", len(events))
	}
	if n := f.appends.Load(); n > 3 {
		t.Fatalf("%d appends for 50 events: they weren't batched", n)
	}
}

// A failed write keeps its events: Flush reports it, and once the store is
// back, the next Flush stores everything, in order.
func TestFailedWriteLosesNothing(t *testing.T) {
	f := newFlaky()
	rec := start(t, f)
	f.set(func(f *flaky) { f.fail = true })
	if err := rec.Record(session.Report, "x"); err != nil {
		t.Fatal(err)
	}
	if err := rec.Flush(ctx); !errors.Is(err, errDown) {
		t.Fatalf("flush: err = %v, want the write error", err)
	}
	if err := rec.Record(session.Report, "y"); err != nil {
		t.Fatalf("record while the store is down: %v", err)
	}
	f.set(func(f *flaky) { f.fail = false })
	if err := rec.Flush(ctx); err != nil {
		t.Fatalf("flush once the store is back: %v", err)
	}
	_, events := stored(t, f, rec)
	if len(events) != 2 || string(events[0].Body) != `"x"` || events[1].Seq != 2 {
		t.Fatalf("events = %+v", events)
	}
	if err := rec.End(ctx); err != nil {
		t.Fatal(err)
	}
}

// A write that landed but whose answer was lost is retried without storing
// anything twice. (Whether Flush sees the error depends on whether the writer
// failed before Flush asked for its own attempt; either way nothing is lost.)
func TestRetryAfterLostAnswerStoresOnce(t *testing.T) {
	f := newFlaky()
	rec := start(t, f)
	f.set(func(f *flaky) { f.loseNext = true })
	_ = rec.Record(session.Report, "x")
	if err := rec.Flush(ctx); err != nil && !errors.Is(err, errDown) {
		t.Fatalf("flush: %v", err)
	}
	_ = rec.Record(session.Report, "y")
	if err := rec.End(ctx); err != nil {
		t.Fatal(err)
	}
	if _, events := stored(t, f, rec); len(events) != 2 {
		t.Fatalf("%d events stored, want 2", len(events))
	}
	if n := f.appends.Load(); n < 2 {
		t.Fatalf("%d appends: the lost answer wasn't retried", n)
	}
}

// Flush and End give up at their deadline even with a stuck write and a long
// queue behind it.
func TestFlushAndEndHonourDeadlineWithAFullQueue(t *testing.T) {
	f := newFlaky()
	rec := start(t, f)
	f.block()
	for i := range 1000 {
		_ = rec.Record(session.ToolCall, i)
	}
	began := time.Now()
	if err := rec.Flush(deadline(t, 20*time.Millisecond)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("flush: err = %v", err)
	}
	if err := rec.End(deadline(t, 20*time.Millisecond)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("end: err = %v", err)
	}
	if took := time.Since(began); took > time.Second {
		t.Fatalf("flush and end took %v past their 20ms deadlines", took)
	}
	f.release()
	if err := rec.End(ctx); err != nil {
		t.Fatal(err)
	}
}

// An End that timed out leaves the run going and its tool results in place; once the store recovers, End again finishes the job.
func TestEndRecoversAfterTimeout(t *testing.T) {
	f := newFlaky()
	rec := start(t, f)
	f.block()
	_ = rec.Record(session.ToolResult, "customer data")
	_ = rec.Record(session.Report, "refunded 1")
	if err := rec.End(deadline(t, 20*time.Millisecond)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("end: err = %v", err)
	}
	if err := rec.Record(session.Report, "late"); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("record after End: err = %v, want ErrEnded", err)
	}
	if state, _ := stored(t, f, rec); state.Ended {
		t.Fatalf("the run ended while End hadn't finished")
	}
	f.release()
	if err := rec.End(ctx); err != nil {
		t.Fatalf("second End: %v", err)
	}
	state, events := stored(t, f, rec)
	if !state.Ended || len(events) != 1 || events[0].Kind != session.Report {
		t.Fatalf("after End: %+v, %+v", state, events)
	}
	if err := rec.End(ctx); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("End after it succeeded: err = %v", err)
	}
}

// An End waiting for another End still gives up at its own deadline.
func TestConcurrentEndHonoursItsDeadline(t *testing.T) {
	f := newFlaky()
	rec := start(t, f)
	f.block()
	_ = rec.Record(session.Report, "x")
	first := make(chan error, 1)
	go func() { first <- rec.End(ctx) }()
	time.Sleep(10 * time.Millisecond) // let the first End take its turn
	began := time.Now()
	if err := rec.End(deadline(t, 10*time.Millisecond)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second End: err = %v, want its deadline", err)
	}
	if took := time.Since(began); took > time.Second {
		t.Fatalf("second End took %v past its 10ms deadline", took)
	}
	f.release()
	if err := <-first; err != nil {
		t.Fatalf("first End: %v", err)
	}
}

// A Store.EndRun that fails once is retried by the next End.
func TestEndRecoversAfterStoreEndFails(t *testing.T) {
	f := newFlaky()
	rec := start(t, f)
	_ = rec.Record(session.ToolResult, "customer data")
	f.set(func(f *flaky) { f.endFails = 1 })
	if err := rec.End(ctx); !errors.Is(err, errDown) {
		t.Fatalf("end: err = %v", err)
	}
	if err := rec.End(ctx); err != nil {
		t.Fatalf("second End: %v", err)
	}
	if state, events := stored(t, f, rec); !state.Ended || len(events) != 0 {
		t.Fatalf("after End: %+v, %d events", state, len(events))
	}
}

// A Store.Hold that fails is retried by the next Pause; until then nothing
// more is recorded, and the run isn't held.
func TestPauseRecoversAfterStoreHoldFails(t *testing.T) {
	f := newFlaky()
	rec := start(t, f)
	_ = rec.Record(session.ToolResult, "customer data")
	f.set(func(f *flaky) { f.holdFails = 1 })
	if err := rec.Pause(ctx); !errors.Is(err, errDown) {
		t.Fatalf("pause: err = %v", err)
	}
	if err := rec.Record(session.Report, "late"); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("record after a failed Pause: err = %v, want ErrEnded", err)
	}
	if state, _ := stored(t, f, rec); state.Held {
		t.Fatal("held although Hold failed")
	}
	if err := rec.Pause(ctx); err != nil {
		t.Fatalf("second Pause: %v", err)
	}
	if state, events := stored(t, f, rec); !state.Held || state.Ended || len(events) != 1 {
		t.Fatalf("after Pause: %+v, %d events: what the run needs stays until it ends", state, len(events))
	}
	if err := rec.End(ctx); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("End after Pause: err = %v, want ErrEnded: the run ends from a resumed Recording", err)
	}
}

// Events read run by run, in the order the runs started; a run whose first
// events expired still sorts by its earliest left.
func TestSortEvents(t *testing.T) {
	at := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	ev := func(run string, seq int64, sec int) session.Event {
		return session.Event{Run: run, Seq: seq, Time: at.Add(time.Duration(sec) * time.Second)}
	}
	events := []session.Event{ev("a", 2, 5), ev("b", 1, 1), ev("a", 3, 6), ev("c", 4, 3), ev("b", 2, 4), ev("a", 1, 2)}
	session.SortEvents(events)
	var got []string
	for _, e := range events {
		got = append(got, fmt.Sprintf("%s%d", e.Run, e.Seq))
	}
	if want := []string{"b1", "b2", "a1", "a2", "a3", "c4"}; !slices.Equal(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

// Recording an event is on every step of the run loop: it has to stay cheap.
func BenchmarkRecord(b *testing.B) {
	r, _ := session.NewRecorder(memory.New(), session.DefaultPolicy(time.Hour))
	s, err := r.Open(ctx, "t1", "refunds", session.KindRun)
	if err != nil {
		b.Fatal(err)
	}
	rec, err := r.Start(ctx, s, "r1", time.Now())
	if err != nil {
		b.Fatal(err)
	}
	body := map[string]string{"tool": "list_charges", "args": `{"limit":10}`}
	b.ReportAllocs()
	for b.Loop() {
		if err := rec.Record(session.ToolCall, body); err != nil {
			b.Fatal(err)
		}
	}
	if err := rec.End(ctx); err != nil {
		b.Fatal(err)
	}
}

// Resuming a paused run is on every continue: one read of the run's state.
func BenchmarkResume(b *testing.B) {
	store := memory.New()
	r, _ := session.NewRecorder(store, session.DefaultPolicy(time.Hour))
	s, err := r.Open(ctx, "t1", "refunds", session.KindRun)
	if err != nil {
		b.Fatal(err)
	}
	rec, err := r.Start(ctx, s, "r1", time.Now())
	if err != nil {
		b.Fatal(err)
	}
	for range 20 {
		_ = rec.Record(session.AgentMessage, "turn")
	}
	if err := rec.Pause(ctx); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		more, err := r.Resume(ctx, s, "r1")
		if err != nil {
			b.Fatal(err)
		}
		if err := more.Pause(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
