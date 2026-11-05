package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool"
)

// stuckRead is a read tool whose calls never return while stuck is open:
// the invocation stopped (killed, or timed out) while the run only read.
// Each call closes the next of inFlow, so a test knows it's in flight.
type stuckRead struct {
	mu     sync.Mutex
	inFlow []chan struct{}
	stuck  chan struct{}
	calls  int
	// through, once set, answers every call at once.
	through bool
}

func newStuckRead(attempts int) *stuckRead {
	s := &stuckRead{stuck: make(chan struct{})}
	for range attempts {
		s.inFlow = append(s.inFlow, make(chan struct{}))
	}
	return s
}
func (s *stuckRead) Spec() model.ToolSpec {
	return model.ToolSpec{Name: "stripe_lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (s *stuckRead) Access() tool.Access { return tool.Read }
func (s *stuckRead) Gateway() bool       { return true }
func (s *stuckRead) Call(context.Context, json.RawMessage) (tool.Result, error) {
	s.mu.Lock()
	n := s.calls
	s.calls++
	through := s.through
	s.mu.Unlock()
	if through || n >= len(s.inFlow) {
		return tool.Result{Text: `{"id":"ch_1","paid":true}`}, nil
	}
	close(s.inFlow[n])
	<-s.stuck
	return tool.Result{}, errors.New("the invocation is gone")
}

// A run whose invocation stops before any turn acted (here, in a look-up)
// sent nothing: the sweep restarts it from its start, as it carries on a
// crashed acting turn, and the owner's ask isn't lost. Its work reads as
// retrying meanwhile, its input is on the record once, and its restarted
// turns cost on the run as its own.
func TestARunKilledBeforeItActedIsRestartedFromItsStart(t *testing.T) {
	// The scripted model: the first attempt's look-up, then the restarted attempt's look-up and answer.
	f := newFixture(t, use("c1", "stripe_lookup", `{"charge":"ch_1"}`))
	s := newStuckRead(1)
	t.Cleanup(func() { close(s.stuck) })
	f.r.Builder.Connect = func(context.Context, string, string) ([]tool.Tool, error) { return []tool.Tool{s}, nil }
	r := f.start(server.Start{Input: "is order 42 paid?"})
	go func() { _, _ = f.r.Execute(ctx, "t1", r.ID, nil) }()
	<-s.inFlow[0]
	if stuck := f.get(r.ID); stuck.Status != run.Running || stuck.Acting != nil {
		t.Fatalf("while it crashed: %+v, want RUNNING with no acting turn", stuck)
	}
	// The invocation is gone: its lease runs out, and the run says so.
	f.clock.add(run.LeaseSpan + time.Second)
	if w := f.get(r.ID).WorkAt(f.clock.now()); w.State != run.WorkRetrying || w.Reason != run.Stopped {
		t.Fatalf("past its lease: %+v", w)
	}
	fresh := f.newRunner()
	fresh.Builder.Connect = f.r.Builder.Connect
	sw := sweeper(f, fresh)
	f.model.add(use("c2", "stripe_lookup", `{"charge":"ch_1"}`), say("Order 42 is paid."))
	restart(t, f, sw, r.ID, run.ResumeWaits[0])
	got := f.get(r.ID)
	if got.Status != run.Completed || got.Output != "Order 42 is paid." || got.Resumes != 1 || got.Paused != nil {
		t.Fatalf("restarted to %+v", got)
	}
	if s.calls != 2 {
		t.Fatalf("the look-up was made %d times, want once per attempt", s.calls)
	}
	// The model was asked again from the owner's input, as the first time.
	reqs := f.model.requests()
	last := reqs[len(reqs)-1].Messages
	if got := texts(last); !slices.Contains(got, "user: is order 42 paid?") {
		t.Fatalf("the restarted turn saw %q", got)
	}
	// The owner's message is on the record once, and the run's cost is its restarted turns'.
	_, events, err := f.store.ReadRun(ctx, "t1", r.Session, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	owner := 0
	for _, e := range events {
		if e.Kind == session.OwnerMessage {
			owner++
		}
	}
	if owner != 1 {
		t.Fatalf("the owner's message is on the record %d times", owner)
	}
	plain := f.start(server.Start{Input: "is order 42 paid?"})
	f.model.add(use("c3", "stripe_lookup", `{"charge":"ch_1"}`), say("Order 42 is paid."))
	if _, err := fresh.Execute(ctx, "t1", plain.ID, nil); err != nil {
		t.Fatal(err)
	}
	if want := f.get(plain.ID).Cost; got.Cost != want || got.Cost == 0 {
		t.Fatalf("the restarted run cost %d, a plain one %d", got.Cost, want)
	}
	// Nothing is left for a later sweep.
	if n, err := sw.Sweep(ctx, f.clock.now(), 10); err != nil || n != 0 {
		t.Fatalf("a second sweep: %d, %v", n, err)
	}
}

// Two sweeps that find the same stopped run restart it once: the store's
// compare-and-set lets one through, and the other finds it moved on.
func TestAStoppedRunIsRestartedOnceUnderConcurrentSweeps(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_lookup", `{"charge":"ch_1"}`), say("Order 42 is paid."))
	s := newStuckRead(1)
	t.Cleanup(func() { close(s.stuck) })
	f.r.Builder.Connect = func(context.Context, string, string) ([]tool.Tool, error) { return []tool.Tool{s}, nil }
	r := f.start(server.Start{Input: "is order 42 paid?"})
	go func() { _, _ = f.r.Execute(ctx, "t1", r.ID, nil) }()
	<-s.inFlow[0]
	f.clock.add(run.LeaseSpan + time.Second)
	var continued atomic.Int32
	sweep := func() run.Sweeper {
		return run.Sweeper{Runs: f.runs, Ended: f.r.Ended, Hold: f.r.Held, Continue: func(context.Context, string, string) error { continued.Add(1); return nil }}
	}
	var wg sync.WaitGroup
	var marked atomic.Int32
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := sweep().Sweep(ctx, f.clock.now(), 10)
			if err != nil {
				t.Error(err)
			}
			marked.Add(int32(n))
		}()
	}
	wg.Wait()
	got := f.get(r.ID)
	if marked.Load() != 1 || got.Status != run.Paused || got.Resumes != 1 || !got.Retrying() {
		t.Fatalf("marked by %d sweeps, now %+v", marked.Load(), got)
	}
	// Its restart, when due, is one continue, however many sweeps look.
	f.clock.add(run.ResumeWaits[0] + time.Second)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := sweep().Sweep(ctx, f.clock.now(), 10); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if continued.Load() != 1 {
		t.Fatalf("continued %d times", continued.Load())
	}
}

// A run that stops before it acted each time it goes gets run.MaxResumes
// restarts, then is the owner's (run.NeedsYou), never restarted for ever.
func TestARunThatKeepsStoppingBeforeItActsNeedsTheOwner(t *testing.T) {
	f := newFixture(t)
	for range run.MaxResumes + 1 {
		f.model.add(use("c1", "stripe_lookup", `{"charge":"ch_1"}`)) // every attempt looks up, and stops there
	}
	s := newStuckRead(run.MaxResumes + 1)
	t.Cleanup(func() { close(s.stuck) })
	f.r.Builder.Connect = func(context.Context, string, string) ([]tool.Tool, error) { return []tool.Tool{s}, nil }
	r := f.start(server.Start{Input: "is order 42 paid?"})
	go func() { _, _ = f.r.Execute(ctx, "t1", r.ID, nil) }()
	<-s.inFlow[0]
	fresh := f.newRunner()
	fresh.Builder.Connect = f.r.Builder.Connect
	sw := run.Sweeper{Runs: f.runs, Ended: fresh.Ended, Hold: fresh.Held, Continue: func(ctx context.Context, tenant, id string) error {
		go func() { _, _ = fresh.Execute(ctx, tenant, id, nil) }()
		return nil
	}}
	for i := range run.MaxResumes {
		f.clock.add(run.LeaseSpan + time.Second)
		if n, err := sw.Sweep(ctx, f.clock.now(), 10); err != nil || n != 1 {
			t.Fatalf("restart %d found: %d, %v", i+1, n, err)
		}
		if got := f.get(r.ID); !got.Retrying() || got.Resumes != i+1 {
			t.Fatalf("restart %d: %+v", i+1, got)
		}
		f.clock.add(run.ResumeWaits[i] + time.Second)
		if n, err := sw.Sweep(ctx, f.clock.now(), 10); err != nil || n != 1 {
			t.Fatalf("restart %d continued: %d, %v", i+1, n, err)
		}
		<-s.inFlow[i+1] // the restarted attempt is in flight, and stops too
	}
	f.clock.add(run.LeaseSpan + time.Second)
	if n, err := sw.Sweep(ctx, f.clock.now(), 10); err != nil || n != 1 {
		t.Fatalf("after the last restart: %d, %v", n, err)
	}
	if got := f.get(r.ID); got.Status != run.Error || got.Error != run.NeedsYou || got.WorkAt(f.clock.now()).State != run.WorkNeedsYou {
		t.Fatalf("after %d restarts: %+v", run.MaxResumes, got)
	}
}
