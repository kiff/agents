package runner_test

import (
	"context"
	"testing"
	"time"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/server"
)

// A run stored PENDING in the background whose invocation never claims it
// (the dispatch was lost) is work the owner asked for: the sweep dispatches
// it again after run.PendingTimeout, up to run.MaxResumes times, then ends
// it as needing the owner (run.NeverStarted), never "no invocation claimed
// it in time" with nothing tried.
func TestAPendingRunNobodyClaimedIsDispatchedAgain(t *testing.T) {
	f := newFixture(t, say("Done."))
	r := f.start(server.Start{Input: "go"})
	if got := f.get(r.ID); got.Status != run.Pending {
		t.Fatalf("stored as %s", got.Status)
	}
	f.clock.add(run.PendingTimeout + run.LeaseGrace + time.Second)
	continued := 0
	sw := run.Sweeper{Runs: f.runs, Ended: f.r.Ended, Hold: f.r.Held, Continue: func(ctx context.Context, tenant, id string) error {
		continued++
		_, err := f.r.Execute(ctx, tenant, id, nil)
		return err
	}}
	if _, err := sw.Sweep(ctx, f.clock.now(), 10); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got := f.get(r.ID)
	if got.Status == run.Error {
		t.Fatalf("ended ERROR %q with %d dispatches: the owner's work is lost", got.Error, continued)
	}
	if continued != 1 || got.Status != run.Completed {
		t.Fatalf("after the sweep: %s, dispatched %d times; want it run once and done", got.Status, continued)
	}

	// A dispatch lost every time: three tries, then the owner's.
	lost := f.start(server.Start{Input: "go"})
	dropped := 0
	sw.Continue = func(context.Context, string, string) error { dropped++; return nil }
	for i := range run.MaxResumes {
		f.clock.add(run.PendingTimeout + time.Second)
		if _, err := sw.Sweep(ctx, f.clock.now(), 10); err != nil {
			t.Fatal(err)
		}
		if got := f.get(lost.ID); got.Status != run.Pending || got.Resumes != i+1 || dropped != i+1 {
			t.Fatalf("try %d: %s, resumes %d, dispatched %d", i+1, got.Status, got.Resumes, dropped)
		}
	}
	f.clock.add(run.PendingTimeout + time.Second)
	if _, err := sw.Sweep(ctx, f.clock.now(), 10); err != nil {
		t.Fatal(err)
	}
	if got := f.get(lost.ID); got.Status != run.Error || got.Error != run.NeverStarted || got.WorkAt(f.clock.now()).State != run.WorkNeedsYou || dropped != run.MaxResumes {
		t.Fatalf("after %d lost dispatches: %+v (work %+v)", run.MaxResumes, got, got.WorkAt(f.clock.now()))
	}
}

// A RUNNING run whose lease ran out stopped: its invocation is gone, and
// until the sweep carries it on its work says so (retrying, from its last
// safe step), never "working".
func TestARunWhoseLeaseRanOutSaysItStopped(t *testing.T) {
	now := time.Now().UTC()
	r := run.Run{ID: "r1", Status: run.Running, Lease: now.Add(-time.Second), Acting: &run.Turn{}}
	if w := r.WorkAt(now); w.State != run.WorkRetrying || w.Reason != run.Stopped || w.Try != 1 || w.Of != run.MaxResumes {
		t.Fatalf("a run whose lease ran out says %+v", w)
	}
	if w := r.WorkAt(now.Add(-2 * time.Second)); w.State != "" {
		t.Fatalf("inside its lease: %+v", w)
	}
	// With no acting turn yet, the same: it restarts from its start.
	r.Acting = nil
	if w := r.WorkAt(now); w.State != run.WorkRetrying || w.Reason != run.Stopped {
		t.Fatalf("with no acting turn: %+v", w)
	}
}

// A tool call that takes longer than the lease's span (a slow vendor, a
// browser checkout) is never taken for a dead invocation: the heartbeat
// renews the lease while the call is in flight, the sweep finds nothing
// due, and the run finishes once.
func TestALongCallIsNeverTakenForADeadInvocation(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_refund", `{}`), say("done"))
	f.r.Heartbeat = time.Millisecond
	release := make(chan struct{})
	inFlow := make(chan struct{})
	f.refund.onCall = func() { close(inFlow); <-release }
	r := f.start(server.Start{Input: "go"})
	done := make(chan error, 1)
	go func() { _, err := f.r.Execute(ctx, "t1", r.ID, nil); done <- err }()
	<-inFlow
	sw := run.Sweeper{Runs: f.runs, Ended: f.r.Ended, Hold: f.r.Held, Continue: func(context.Context, string, string) error { t.Error("the sweep continued a live run"); return nil }}
	for range 5 {
		f.clock.add(time.Minute)
		// The heartbeat beats in real time: wait for the beat after the jump, which renews the lease
		// to its full span again (the clock jumps here; in life it moves and the beats are 30 s apart).
		deadline := time.Now().Add(2 * time.Second)
		for f.get(r.ID).Lease.Before(f.clock.now().Add(run.LeaseSpan-time.Second)) && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
		if got := f.get(r.ID); !got.Lease.After(f.clock.now()) || got.WorkAt(f.clock.now()).State != "" {
			t.Fatalf("a minute into the call: lease %v at %v, work %+v", got.Lease, f.clock.now(), got.WorkAt(f.clock.now()))
		}
		if n, err := sw.Sweep(ctx, f.clock.now(), 10); err != nil || n != 0 {
			t.Fatalf("the sweep during the call: %d, %v", n, err)
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := f.get(r.ID); got.Status != run.Completed || len(f.refund.trials) != 1 {
		t.Fatalf("after the call: %s, %d calls", got.Status, len(f.refund.trials))
	}
}
