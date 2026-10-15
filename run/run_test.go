package run_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
)

func TestFinalStatuses(t *testing.T) {
	final := map[run.Status]bool{
		run.Pending: false, run.Running: false, run.Paused: false,
		run.Completed: true, run.Error: true, run.Cancelled: true,
	}
	for s, want := range final {
		if s.Final() != want {
			t.Errorf("%s: final %v, want %v", s, s.Final(), want)
		}
	}
}

// A requirement is resolved once KIFF answered it, or once its hold expired:
// expiry means refused, and a held call never runs on silence.
func TestARequirementIsResolvedByAnAnswerOrExpiry(t *testing.T) {
	at := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	r := run.Requirement{CallID: "c1", Tool: "stripe_refund", Operation: "op_1", Expires: at.Add(10 * time.Minute)}
	if r.Resolved(at) {
		t.Fatal("resolved before an answer or expiry")
	}
	if !r.Resolved(at.Add(10 * time.Minute)) {
		t.Fatal("not resolved once its hold expired")
	}
	r.Answered = at.Add(time.Minute)
	if !r.Resolved(at.Add(time.Minute)) {
		t.Fatal("not resolved once answered")
	}
	// No expiry recorded: never resolved by time, only by an answer.
	unknown := run.Requirement{CallID: "c1", Tool: "stripe_refund"}
	if unknown.Resolved(at.Add(365 * 24 * time.Hour)) {
		t.Fatal("a requirement with no expiry and no answer counts as resolved")
	}
	unknown.Answered = at
	if !unknown.Resolved(at) {
		t.Fatal("a requirement with no expiry isn't resolved by an answer")
	}
}

// A paused run stores and reads back whole: it continues from what it stored.
func TestAPausedRunRoundTrips(t *testing.T) {
	at := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	in := run.Run{
		ID: run.NewID(), Tenant: "t1", Agent: "refunds", Session: "s1", Trigger: run.ByOwner, Status: run.Paused,
		Input: "Refund order 12.", Cost: 1200, Turns: 2,
		Requirements: []run.Requirement{{CallID: "c2", Tool: "stripe_refund", Input: json.RawMessage(`{"charge":"ch_1"}`),
			Operation: "op_1", Expires: at.Add(10 * time.Minute)}},
		Paused: &run.Turn{
			Reply: model.Message{Role: model.Assistant, Blocks: []model.Block{
				{Kind: model.ToolUse, CallID: "c1", Tool: "stripe_get", Input: json.RawMessage(`{}`)},
				{Kind: model.ToolUse, CallID: "c2", Tool: "stripe_refund", Input: json.RawMessage(`{"charge":"ch_1"}`)},
			}},
			Results: []model.Block{{Kind: model.ToolResult, CallID: "c1", Result: "{}"}},
		},
		Created: at, Updated: at, Version: 3,
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out run.Run
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip:\n in %+v\nout %+v", in, out)
	}
}

func TestNewIDsDiffer(t *testing.T) {
	if a, b := run.NewID(), run.NewID(); a == b || len(a) != 32 {
		t.Fatalf("ids %q, %q", a, b)
	}
}

// A run claimed for its PENDING wait can always start recording: the sweep
// gives up on it well before the session store would refuse to begin it.
func TestPendingTimeoutIsWithinMaxStartDelay(t *testing.T) {
	if run.PendingTimeout+run.LeaseGrace >= session.MaxStartDelay {
		t.Fatalf("PendingTimeout %v is too close to session.MaxStartDelay %v", run.PendingTimeout, session.MaxStartDelay)
	}
}

func TestDue(t *testing.T) {
	at := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	r := run.Run{Status: run.Pending, Created: at, Lease: at.Add(time.Hour)}
	if !r.Due().Equal(at.Add(run.PendingTimeout)) {
		t.Fatalf("pending: due %v", r.Due())
	}
	r.Status = run.Running
	if !r.Due().Equal(at.Add(time.Hour)) {
		t.Fatalf("running: due %v, want its lease", r.Due())
	}
	for _, s := range []run.Status{run.Paused, run.Completed, run.Error, run.Cancelled} {
		r.Status = s
		if !r.Due().IsZero() {
			t.Fatalf("%s: due %v, want none", s, r.Due())
		}
	}
}

// A cancelled run isn't claimed: it stops before it starts.
func TestACancelledRunIsNotClaimed(t *testing.T) {
	ctx := context.Background()
	runs := memory.NewRuns()
	r := run.New("t1", "refunds", "s1", run.ByOwner, "go", time.Now())
	if err := runs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	r, err := run.Change(ctx, runs, r, time.Now(), func(n *run.Run) error { n.Cancel = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.Claim(ctx, runs, "t1", r.ID, "i1", time.Now().Add(time.Minute), time.Now()); !errors.Is(err, run.ErrTransition) {
		t.Fatalf("claiming a cancelled run: err = %v, want ErrTransition", err)
	}
}

// A paused run asked to stop is still claimed once KIFF has resolved it, so
// it can record its held calls' outcomes and stop.
func TestACancelledPausedRunIsClaimedOnceResolved(t *testing.T) {
	ctx := context.Background()
	runs := memory.NewRuns()
	now := time.Now()
	r := run.New("t1", "refunds", "s1", run.ByOwner, "go", now)
	if err := runs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	r, err := run.Claim(ctx, runs, "t1", r.ID, "i1", now.Add(time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	r, err = run.Change(ctx, runs, r, now, func(n *run.Run) error {
		n.Status, n.Cancel = run.Paused, true
		n.Requirements = []run.Requirement{{CallID: "c1", Tool: "stripe_refund"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.Claim(ctx, runs, "t1", r.ID, "i2", now.Add(time.Minute), now); !errors.Is(err, run.ErrNotReady) {
		t.Fatalf("claiming it before KIFF answered: %v", err)
	}
	if _, err := run.Change(ctx, runs, r, now, func(n *run.Run) error { n.Requirements[0].Answered = now; return nil }); err != nil {
		t.Fatal(err)
	}
	if got, err := run.Claim(ctx, runs, "t1", r.ID, "i2", now.Add(time.Minute), now); err != nil || got.Status != run.Running || !got.Cancel {
		t.Fatalf("claimed %+v, %v; want it running, still asked to stop", got, err)
	}
}

// Starting a run is stored, then claimed: twice the store, on every message.
func BenchmarkStart(b *testing.B) {
	ctx := context.Background()
	runs := memory.NewRuns()
	now := time.Now()
	b.ReportAllocs()
	for b.Loop() {
		r := run.New("t1", "refunds", "s1", run.ByOwner, "refund order 12", now)
		if err := runs.Create(ctx, r); err != nil {
			b.Fatal(err)
		}
		if _, err := run.Claim(ctx, runs, "t1", r.ID, "i1", now.Add(15*time.Minute), now); err != nil {
			b.Fatal(err)
		}
	}
}

// Continuing a paused run is a claim of it once its requirements resolved.
func BenchmarkContinue(b *testing.B) {
	ctx := context.Background()
	runs := memory.NewRuns()
	now := time.Now()
	r := run.New("t1", "refunds", "s1", run.ByOwner, "refund order 12", now)
	if err := runs.Create(ctx, r); err != nil {
		b.Fatal(err)
	}
	turn := &run.Turn{Reply: model.Message{Role: model.Assistant, Blocks: []model.Block{{Kind: model.ToolUse, CallID: "c1", Tool: "stripe_refund"}}}}
	held := []run.Requirement{{CallID: "c1", Tool: "stripe_refund", Operation: "op_1", Expires: now.Add(time.Hour), Answered: now}}
	b.ReportAllocs()
	for b.Loop() {
		r, err := run.Claim(ctx, runs, "t1", r.ID, "i1", now.Add(15*time.Minute), now)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := run.Save(ctx, runs, r, now, func(n *run.Run) error {
			n.Status, n.Requirements, n.Paused = run.Paused, held, turn
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}
}
