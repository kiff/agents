package run_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
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
