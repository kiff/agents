package session_test

import (
	"context"
	"errors"
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
	p[session.ToolResult] = session.Rule{Keep: session.UntilEnd, Retention: time.Hour}
	if err := p.Check(); err == nil {
		t.Fatal("retention on an until-end kind was accepted")
	}
	p[session.ToolResult] = session.Rule{Keep: session.Kept, Retention: -time.Hour}
	if err := p.Check(); err == nil {
		t.Fatal("negative retention was accepted")
	}
}

func TestDefaultPolicyKeepsToolResultsOnlyForTheRun(t *testing.T) {
	p := session.DefaultPolicy(30 * 24 * time.Hour)
	if p[session.ToolResult].Keep != session.UntilEnd {
		t.Fatalf("tool results: %+v", p[session.ToolResult])
	}
	if p[session.OwnerMessage] != (session.Rule{Keep: session.Kept, Retention: 30 * 24 * time.Hour}) {
		t.Fatalf("owner messages: %+v", p[session.OwnerMessage])
	}
}

// flaky is a store a test controls: appends wait at a gate the test opens, and
// appends or ends can be made to fail.
type flaky struct {
	*memory.Store
	appends atomic.Int64

	mu       sync.Mutex
	gate     chan struct{} // appends wait until it's closed
	fail     bool          // appends fail without storing
	loseNext bool          // the next append stores, then fails (its answer is lost)
	endFails int           // this many Ends fail
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

func (f *flaky) Append(ctx context.Context, tenant, id string, events []session.Event) error {
	f.appends.Add(1)
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
	if err := f.Store.Append(ctx, tenant, id, events); err != nil {
		return err
	}
	if lose {
		return errDown
	}
	return nil
}

func (f *flaky) End(ctx context.Context, tenant, id string, status session.Status) error {
	f.mu.Lock()
	failing := f.endFails > 0
	if failing {
		f.endFails--
	}
	f.mu.Unlock()
	if failing {
		return errDown
	}
	return f.Store.End(ctx, tenant, id, status)
}

func start(t *testing.T, f *flaky) *session.Recording {
	t.Helper()
	r, err := session.NewRecorder(f, session.DefaultPolicy(0))
	if err != nil {
		t.Fatal(err)
	}
	rec, err := r.Start(ctx, "t1", "refunds", session.KindRun)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func stored(t *testing.T, f *flaky, rec *session.Recording) (session.Session, []session.Event) {
	t.Helper()
	s, events, err := f.Read(ctx, "t1", rec.Session().ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, events
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
		if err := rec.Record(session.ToolCall, i); err != nil {
			t.Fatal(err)
		}
	}
	f.release()
	if err := rec.End(ctx, session.Ended); err != nil {
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
	if err := rec.End(ctx, session.Ended); err != nil {
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
	if err := rec.End(ctx, session.Ended); err != nil {
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
	if err := rec.End(deadline(t, 20*time.Millisecond), session.Ended); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("end: err = %v", err)
	}
	if took := time.Since(began); took > time.Second {
		t.Fatalf("flush and end took %v past their 20ms deadlines", took)
	}
	f.release()
	if err := rec.End(ctx, session.Ended); err != nil {
		t.Fatal(err)
	}
}

// An End that timed out leaves the session running and its tool results in
// place; once the store recovers, End again finishes the job.
func TestEndRecoversAfterTimeout(t *testing.T) {
	f := newFlaky()
	rec := start(t, f)
	f.block()
	_ = rec.Record(session.ToolResult, "customer data")
	_ = rec.Record(session.Report, "refunded 1")
	if err := rec.End(deadline(t, 20*time.Millisecond), session.Ended); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("end: err = %v", err)
	}
	if err := rec.Record(session.Report, "late"); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("record after End: err = %v, want ErrEnded", err)
	}
	if s, _ := stored(t, f, rec); s.Status != session.Running {
		t.Fatalf("status = %s while End hadn't finished", s.Status)
	}
	f.release()
	if err := rec.End(ctx, session.Ended); err != nil {
		t.Fatalf("second End: %v", err)
	}
	s, events := stored(t, f, rec)
	if s.Status != session.Ended || len(events) != 1 || events[0].Kind != session.Report {
		t.Fatalf("after End: %s, %+v", s.Status, events)
	}
	if err := rec.End(ctx, session.Ended); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("End after it succeeded: err = %v", err)
	}
}

// A Store.End that fails once is retried by the next End.
func TestEndRecoversAfterStoreEndFails(t *testing.T) {
	f := newFlaky()
	rec := start(t, f)
	_ = rec.Record(session.ToolResult, "customer data")
	f.set(func(f *flaky) { f.endFails = 1 })
	if err := rec.End(ctx, session.Ended); !errors.Is(err, errDown) {
		t.Fatalf("end: err = %v", err)
	}
	if err := rec.End(ctx, session.Ended); err != nil {
		t.Fatalf("second End: %v", err)
	}
	if s, events := stored(t, f, rec); s.Status != session.Ended || len(events) != 0 {
		t.Fatalf("after End: %s, %d events", s.Status, len(events))
	}
}

// Recording an event is on every step of the run loop: it has to stay cheap.
func BenchmarkRecord(b *testing.B) {
	r, _ := session.NewRecorder(memory.New(), session.DefaultPolicy(time.Hour))
	rec, err := r.Start(ctx, "t1", "refunds", session.KindRun)
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
	if err := rec.End(ctx, session.Ended); err != nil {
		b.Fatal(err)
	}
}
