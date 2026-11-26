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

// A message with the client's key is kept once on its run: the same key and
// text again adds nothing (repeat), even once the run has taken it; another
// text or writer under the key is ErrDedupConflict; another run never sees
// the key; and with no key every message is kept, as before.
func TestSteerKeyedKeepsAKeyedMessageOnce(t *testing.T) {
	ctx := context.Background()
	runs := memory.NewRuns()
	at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	ana := run.Requester{Kind: run.ByMember, ID: "ana"}
	r := run.New("t1", "refunds", "s1", run.ByOwner, "refund order 12", at)
	other := run.New("t1", "refunds", "s1", run.ByOwner, "refund order 13", at)
	for _, x := range []run.Run{r, other} {
		if err := runs.Create(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := run.Claim(ctx, runs, "t1", r.ID, "i1", at.Add(time.Minute), at); err != nil {
		t.Fatal(err)
	}
	got, repeat, err := run.SteerKeyed(ctx, runs, "t1", r.ID, "only the €20 one", "k1", ana, at)
	if err != nil || repeat || len(got.Steering) != 1 || got.Steering[0].Key != "k1" {
		t.Fatalf("first: %+v, repeat %v, %v", got.Steering, repeat, err)
	}
	// Taken by the run, then sent again: still the same message, kept once.
	if _, err := run.Save(ctx, runs, got, at, func(n *run.Run) error { n.Steering[0].Taken = true; return nil }); err != nil {
		t.Fatal(err)
	}
	got, repeat, err = run.SteerKeyed(ctx, runs, "t1", r.ID, "only the €20 one", "k1", ana, at)
	if err != nil || !repeat || len(got.Steering) != 1 {
		t.Fatalf("again: %+v, repeat %v, %v", got.Steering, repeat, err)
	}
	if _, _, err := run.SteerKeyed(ctx, runs, "t1", r.ID, "all of them", "k1", ana, at); !errors.Is(err, run.ErrDedupConflict) {
		t.Fatalf("another text: %v, want ErrDedupConflict", err)
	}
	if _, _, err := run.SteerKeyed(ctx, runs, "t1", r.ID, "only the €20 one", "k1", run.Requester{Kind: run.ByMember, ID: "luis"}, at); !errors.Is(err, run.ErrDedupConflict) {
		t.Fatalf("another writer: %v, want ErrDedupConflict", err)
	}
	if got, repeat, err := run.SteerKeyed(ctx, runs, "t1", other.ID, "all of them", "k1", ana, at); err != nil || repeat || len(got.Steering) != 1 {
		t.Fatalf("another run: %+v, repeat %v, %v", got.Steering, repeat, err)
	}
	for range 2 {
		if _, _, err := run.SteerKeyed(ctx, runs, "t1", other.ID, "and the fee", "", ana, at); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := runs.Get(ctx, "t1", other.ID); len(got.Steering) != 3 {
		t.Fatalf("without a key: %d messages, want 3", len(got.Steering))
	}
}

// What a run may answer its request with is its caller's, fixed at its start:
// a change can set the outcome it answered, never its list of outcomes.
func TestARunsOutcomesAreFixed(t *testing.T) {
	ctx := context.Background()
	runs := memory.NewRuns()
	now := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	r := run.New("t1", "rae", "s1", run.ByOwner, "refund 42", now)
	r.Outcomes = []string{"refunded", "refused"}
	if err := runs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := run.Change(ctx, runs, r, now, func(n *run.Run) error { n.Outcomes = []string{"refunded"}; return nil }); !errors.Is(err, run.ErrFixed) {
		t.Errorf("changing a run's outcomes: %v, want ErrFixed", err)
	}
	got, err := run.Change(ctx, runs, r, now, func(n *run.Run) error { n.Outcome = "refused"; return nil })
	if err != nil || got.Outcome != "refused" {
		t.Errorf("setting the outcome it answered: %q, %v", got.Outcome, err)
	}
}

// Each of our own reasons a run stopped has its code, and only a daily limit
// says when it resets: the next midnight UTC after the run ended, as the day's
// spend counts by the UTC date. A run that ended at 23:59:59 UTC goes on a
// second later; one at 00:00:00 waits the whole day.
func TestADailyLimitSaysWhenItResets(t *testing.T) {
	for err, code := range map[string]string{run.AIUnavailable: "ai_unavailable", run.AILimitAccount: "ai_limit_account", run.AILimitService: "ai_limit_service", "the run failed": ""} {
		if got := run.ErrorCode(err); got != code {
			t.Errorf("run.ErrorCode(%q) = %q, want %q", err, got, code)
		}
	}
	for _, c := range []struct {
		err   string
		ended time.Time
		want  time.Time
	}{
		{run.AILimitAccount, time.Date(2026, 11, 26, 17, 47, 48, 0, time.UTC), time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)},
		{run.AILimitService, time.Date(2026, 11, 26, 23, 59, 59, 0, time.UTC), time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)},
		{run.AILimitAccount, time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)},
		// Paris time, the same instant: still midnight UTC, never the zone's own midnight.
		{run.AILimitAccount, time.Date(2026, 11, 27, 0, 30, 0, 0, time.FixedZone("CET", 3600)), time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)},
		{run.AIUnavailable, time.Date(2026, 11, 26, 17, 0, 0, 0, time.UTC), time.Time{}},
		{"the run failed", time.Date(2026, 11, 26, 17, 0, 0, 0, time.UTC), time.Time{}},
	} {
		if got := run.ResetsAt(run.Run{Error: c.err, Ended: c.ended}); !got.Equal(c.want) {
			t.Errorf("run.ResetsAt(%q, ended %v) = %v, want %v", c.err, c.ended, got, c.want)
		}
	}
	// A run from before Ended was kept: when it was last changed.
	if got := run.ResetsAt(run.Run{Error: run.AILimitService, Updated: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}); !got.Equal(time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("from Updated: %v", got)
	}
	if got := run.ResetsAt(run.Run{Error: run.AILimitService}); !got.IsZero() {
		t.Errorf("with no time at all: %v", got)
	}
}
