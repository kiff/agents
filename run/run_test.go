package run_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
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
	r.Status = run.Paused
	r.Requirements = []run.Requirement{
		{CallID: "c1", Expires: at.Add(2 * time.Hour), Answered: at.Add(time.Minute)},
		{CallID: "c2", Expires: at.Add(time.Hour)},
	}
	if !r.Due().Equal(at.Add(time.Hour + run.ResolvedGrace)) {
		t.Fatalf("paused: due %v, want the last requirement's resolution plus the grace", r.Due())
	}
	r.Requirements[1].Answered = at.Add(3 * time.Hour)
	if !r.Due().Equal(at.Add(3*time.Hour + run.ResolvedGrace)) {
		t.Fatalf("paused, answered after: due %v", r.Due())
	}
	r.Retry = at.Add(5 * time.Hour)
	if !r.Due().Equal(r.Retry) {
		t.Fatalf("paused, continued by the sweep: due %v, want its retry", r.Due())
	}
	r.Retry = at.Add(2 * time.Hour)
	if !r.Due().Equal(at.Add(3*time.Hour + run.ResolvedGrace)) {
		t.Fatalf("paused, retry before its resolution: due %v", r.Due())
	}
	r.Requirements = append(r.Requirements, run.Requirement{CallID: "c3"})
	if !r.Due().IsZero() {
		t.Fatalf("paused on a requirement that never expires: due %v, want none", r.Due())
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

// A paused run keeps its earlier turns (Turn.Before) through every change,
// KIFF's answer to its hold included: without them, the continued run has no
// memory of the look-up it made before the hold, and says it skipped it (the
// R-3005 test).
func TestAPausedRunKeepsItsEarlierTurnsThroughAnAnswer(t *testing.T) {
	ctx := context.Background()
	runs := memory.NewRuns()
	now := time.Now()
	r := run.New("t1", "refunds", "s1", run.ByOwner, "refund ch_1", now)
	r.Status = run.Paused
	before := []model.Message{
		{Role: model.Assistant, Blocks: []model.Block{{Kind: model.ToolUse, CallID: "c1", Tool: "get_charge", Input: json.RawMessage(`{"charge":"ch_1"}`)}}},
		{Role: model.User, Blocks: []model.Block{{Kind: model.ToolResult, CallID: "c1", Result: `{"amount":900}`}}},
	}
	r.Paused = &run.Turn{Reply: model.Message{Role: model.Assistant, Blocks: []model.Block{{Kind: model.ToolUse, CallID: "c2", Tool: "create_refund"}}}, Before: before}
	r.Requirements = []run.Requirement{{CallID: "c2", Tool: "create_refund", Exception: "exc_1", Expires: now.Add(time.Hour)}}
	if err := runs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := run.Answer(ctx, runs, "t1", "exc_1", "rejected", now)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := runs.Get(ctx, "t1", r.ID)
	for what, x := range map[string]run.Run{"answered": got, "stored": stored} {
		if x.Paused == nil || !reflect.DeepEqual(x.Paused.Before, before) {
			t.Errorf("%s: the paused turn's earlier turns are %+v, want the look-up", what, x.Paused)
		}
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

// The owner's messages to a run that hasn't ended are kept on it, in order,
// until it takes them; one that has ended, or was asked to stop, takes none,
// and none past MaxSteering (#102).
func TestSteerKeepsMessagesForARunThatGoes(t *testing.T) {
	ctx := context.Background()
	runs := memory.NewRuns()
	at := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	r := run.New("t1", "refunds", "s1", run.ByOwner, "refund order 12", at)
	if err := runs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	r, err := run.Claim(ctx, runs, "t1", r.ID, "i1", at.Add(time.Minute), at)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"only the €20 one", "and tell me when it's done"} {
		if r, err = run.Steer(ctx, runs, "t1", r.ID, text, at); err != nil {
			t.Fatal(err)
		}
	}
	if r.Status != run.Running || r.Claim != "i1" {
		t.Fatalf("steering moved the run: %+v", r)
	}
	if got := r.Untaken(); !reflect.DeepEqual(got, []string{"only the €20 one", "and tell me when it's done"}) || len(r.Taken()) != 0 {
		t.Fatalf("untaken %q, taken %q", got, r.Taken())
	}
	r, err = run.Save(ctx, runs, r, at, func(n *run.Run) error { n.Steering[0].Taken = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Taken(), []string{"only the €20 one"}) || !reflect.DeepEqual(r.Untaken(), []string{"and tell me when it's done"}) {
		t.Fatalf("taken %q, untaken %q", r.Taken(), r.Untaken())
	}

	for len(r.Steering) < run.MaxSteering {
		if r, err = run.Steer(ctx, runs, "t1", r.ID, "more", at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := run.Steer(ctx, runs, "t1", r.ID, "one too many", at); !errors.Is(err, run.ErrFull) {
		t.Fatalf("past MaxSteering: %v, want ErrFull", err)
	}

	// The text together is bounded too: a run's item has room for it.
	long := run.New("t1", "refunds", "s1", run.ByOwner, "go", at)
	if err := runs.Create(ctx, long); err != nil {
		t.Fatal(err)
	}
	if _, err := run.Steer(ctx, runs, "t1", long.ID, strings.Repeat("m", run.MaxSteeringBytes), at); err != nil {
		t.Fatalf("a message of the whole budget: %v", err)
	}
	if _, err := run.Steer(ctx, runs, "t1", long.ID, "m", at); !errors.Is(err, run.ErrFull) {
		t.Fatalf("past MaxSteeringBytes: %v, want ErrFull", err)
	}

	cancelled, err := run.Change(ctx, runs, r, at, func(n *run.Run) error { n.Cancel = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.Steer(ctx, runs, "t1", cancelled.ID, "wait", at); !errors.Is(err, run.ErrTransition) {
		t.Fatalf("a run asked to stop: %v, want ErrTransition", err)
	}

	done := run.New("t1", "refunds", "s1", run.ByOwner, "go", at)
	done.Status = run.Completed
	if err := runs.Create(ctx, done); err != nil {
		t.Fatal(err)
	}
	if _, err := run.Steer(ctx, runs, "t1", done.ID, "wait", at); !errors.Is(err, run.ErrTransition) {
		t.Fatalf("an ended run: %v, want ErrTransition", err)
	}
	if _, err := run.Steer(ctx, runs, "t1", "nope", "wait", at); !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("no such run: %v", err)
	}
}

// What a run did keeps each call's input while the inputs together stay
// within MaxDoneBytes; past it, each call is still listed, without its input
// (#249).
func TestDoneKeepsEveryCallWithinItsBytes(t *testing.T) {
	big := json.RawMessage(`"` + strings.Repeat("x", run.MaxDoneBytes/2) + `"`)
	var done []run.Done
	for i, id := range []string{"c1", "c2", "c3"} {
		done = run.AddDone(done, run.Done{CallID: id, Tool: "send_email", Input: big, Resource: "r" + id})
		if len(done) != i+1 {
			t.Fatalf("after %s: %d calls", id, len(done))
		}
	}
	if len(done[0].Input) == 0 || done[1].Input != nil || done[2].Input != nil {
		t.Fatalf("inputs kept: %d, %d, %d bytes", len(done[0].Input), len(done[1].Input), len(done[2].Input))
	}
	small := run.Done{CallID: "c4", Tool: "stripe_refund", Input: json.RawMessage(`{"charge":"ch_1"}`)}
	if done = run.AddDone(done, small); string(done[3].Input) != `{"charge":"ch_1"}` || done[2].CallID != "c3" || done[2].Resource != "rc3" {
		t.Fatalf("a small input after the bound: %+v", done[3])
	}
}
