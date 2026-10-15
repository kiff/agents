package session_test

import (
	"context"
	"errors"
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

// slow is a store whose appends take a while, like a network database, and can
// be made to fail.
type slow struct {
	*memory.Store
	delay   time.Duration
	appends atomic.Int64
	fail    atomic.Bool
}

var errDown = errors.New("store down")

func (s *slow) Append(ctx context.Context, tenant, id string, events []session.Event) error {
	s.appends.Add(1)
	time.Sleep(s.delay)
	if s.fail.Load() {
		return errDown
	}
	return s.Store.Append(ctx, tenant, id, events)
}

// The run loop doesn't wait on the store: recording returns at once, and what
// queues during a slow write goes in the next one.
func TestRecordDoesNotWaitAndBatches(t *testing.T) {
	st := &slow{Store: memory.New(), delay: 20 * time.Millisecond}
	r, _ := session.NewRecorder(st, session.DefaultPolicy(0))
	rec, err := r.Start(ctx, "t1", "refunds", session.KindRun)
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	for i := range 50 {
		if err := rec.Record(session.ToolCall, i); err != nil {
			t.Fatal(err)
		}
	}
	if took := time.Since(began); took > 10*time.Millisecond {
		t.Fatalf("recording 50 events took %v: it waited on the store", took)
	}
	if err := rec.End(ctx, session.Ended); err != nil {
		t.Fatal(err)
	}
	_, events, _ := st.Read(ctx, "t1", rec.Session().ID)
	if len(events) != 50 {
		t.Fatalf("%d events stored, want 50", len(events))
	}
	if n := st.appends.Load(); n > 4 {
		t.Fatalf("%d appends for 50 events: they weren't batched", n)
	}
}

// A failed write is never lost: the next Record, Flush or End returns it, and
// the session isn't ended as if everything were stored.
func TestWriteErrorSurfaces(t *testing.T) {
	st := &slow{Store: memory.New()}
	r, _ := session.NewRecorder(st, session.DefaultPolicy(0))
	rec, _ := r.Start(ctx, "t1", "refunds", session.KindRun)
	st.fail.Store(true)
	if err := rec.Record(session.Report, "x"); err != nil {
		t.Fatalf("record queued: %v", err)
	}
	if err := rec.Flush(ctx); !errors.Is(err, errDown) {
		t.Fatalf("flush: err = %v, want the write error", err)
	}
	if err := rec.Record(session.Report, "y"); !errors.Is(err, errDown) {
		t.Fatalf("record after a failed write: err = %v", err)
	}
	if err := rec.End(ctx, session.Ended); !errors.Is(err, errDown) {
		t.Fatalf("end: err = %v", err)
	}
	if s, _, _ := st.Read(ctx, "t1", rec.Session().ID); s.Status != session.Running {
		t.Fatalf("session was ended as %s although an event was lost", s.Status)
	}
}

func TestFlushHonoursContext(t *testing.T) {
	st := &slow{Store: memory.New(), delay: time.Second}
	r, _ := session.NewRecorder(st, session.DefaultPolicy(0))
	rec, _ := r.Start(ctx, "t1", "refunds", session.KindRun)
	_ = rec.Record(session.Report, "x")
	c, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := rec.Flush(c); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("flush: err = %v, want the deadline", err)
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
