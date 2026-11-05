package runner_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool"
)

// waiting is the gateway tool's result for a sent call whose outcome comes
// later, as planque says it: pending, with the event and key to wait for.
func waiting() tool.Result {
	return tool.Result{Text: `{"id":"reply_1"}` + "\nThe outcome comes later: the run waits up to 15 min for the DESK_NEW_TICKET event.", State: tool.StateWaiting,
		Wait: &tool.Wait{Grant: "grt_desk", Event: "DESK_NEW_TICKET", Key: "reply_1", TimeoutS: 900, Words: "The desk answers when the customer replies; the taf waits for that."}}
}

// A sent call whose outcome comes later makes the run wait, as a card does:
// on an event requirement with the grant, the event, the key and a deadline
// from the connection's timeout. The event's payload, answered through
// run.AnswerWait, is the call's result when the run goes on, said to be the
// outcome that came later; a second answer is refused.
func TestACallThatCompletesLaterWaitsForItsEvent(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_refund", `{"ticket":"tk_1","text":"hello"}`), say("I replied; waiting for the customer."), say("The customer answered: thanks."))
	f.refund.result = waiting()
	r := f.start(server.Start{Input: "Reply to tk_1."})
	got, err := f.r.Execute(ctx, "t1", r.ID, nil)
	if err != nil || got.Status != run.Paused || len(got.Requirements) != 1 {
		t.Fatalf("ran to %s with %+v, %v; want paused on the event", got.Status, got.Requirements, err)
	}
	q := got.Requirements[0]
	if !q.Event() || q.Card() || q.Wait == nil || q.Wait.Grant != "grt_desk" || q.Wait.Event != "DESK_NEW_TICKET" || q.Wait.Key != "reply_1" || q.Wait.Words == "" ||
		!q.Expires.Equal(f.clock.now().Add(15*time.Minute)) || q.CallID != "c1" || q.Tool != "stripe_refund" {
		t.Fatalf("waits on %+v (wait %+v); want the event for 15 min", q, q.Wait)
	}
	reqs := f.model.requests()
	if note := texts(reqs[len(reqs)-1].Messages); !slices.Contains(note, "user: "+agent.WaitPauseNote) {
		t.Fatalf("the waiting turn was told %q", note)
	}
	// Still waiting: the run isn't continued.
	if _, err := f.r.Execute(ctx, "t1", r.ID, nil); err == nil {
		t.Fatal("a run still waiting for its event was continued")
	}

	payload := json.RawMessage(`{"ticket":{"id":"tk_1","reply":"reply_1","text":"thanks"}}`)
	if _, err := run.AnswerWait(ctx, f.runs, "t1", r.ID, "c1", payload, f.clock.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := run.AnswerWait(ctx, f.runs, "t1", r.ID, "c1", payload, f.clock.now()); err != run.ErrNotOpen {
		t.Fatalf("a second event: %v; want ErrNotOpen", err)
	}
	if got, err = f.r.Execute(ctx, "t1", r.ID, nil); err != nil || got.Status != run.Completed {
		t.Fatalf("went on to %s, %v", got.Status, err)
	}
	reqs = f.model.requests()
	last := reqs[len(reqs)-1].Messages
	res := last[len(last)-1].Blocks[0]
	if res.Kind != model.ToolResult || res.CallID != "c1" || !strings.Contains(res.Result, "came later, as the DESK_NEW_TICKET event") || !strings.Contains(res.Result, string(payload)) {
		t.Fatalf("the event as the call's result: %+v", res)
	}
	if kinds := f.events(got); !slices.Contains(kinds, session.Called) || slices.Contains(kinds, session.Answered) {
		t.Fatalf("recorded %v; want the call's outcome, no card answer", kinds)
	}
	if lines := f.called(got); !slices.Contains(lines, agent.OutcomeCame) {
		t.Fatalf("the record says %q; want %q", lines, agent.OutcomeCame)
	}
}

// An event that never comes: once the wait runs out, the run goes on with
// a result that says the outcome didn't come in time, never an answer.
func TestAWaitThatRunsOutSaysSo(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_refund", `{"ticket":"tk_1","text":"hello"}`), say("Waiting for the customer."), say("No answer came; I'll tell the owner."))
	f.refund.result = waiting()
	r := f.start(server.Start{Input: "Reply to tk_1."})
	if got, _ := f.r.Execute(ctx, "t1", r.ID, nil); got.Status != run.Paused {
		t.Fatalf("ran to %s", got.Status)
	}
	f.clock.add(15*time.Minute + time.Second)
	// Too late for the event.
	if _, err := run.AnswerWait(ctx, f.runs, "t1", r.ID, "c1", json.RawMessage(`{}`), f.clock.now()); err != run.ErrNotOpen {
		t.Fatalf("a late event: %v; want ErrNotOpen", err)
	}
	got, err := f.r.Execute(ctx, "t1", r.ID, nil)
	if err != nil || got.Status != run.Completed {
		t.Fatalf("went on to %s, %v", got.Status, err)
	}
	reqs := f.model.requests()
	last := reqs[len(reqs)-1].Messages
	res := last[len(last)-1].Blocks[0]
	if !strings.Contains(res.Result, "didn't come in time") || !strings.Contains(res.Result, `{"event":"DESK_NEW_TICKET","timed_out":true}`) {
		t.Fatalf("the timed-out result: %q", res.Result)
	}
	if lines := f.called(got); !slices.Contains(lines, agent.OutcomeNotCame) {
		t.Fatalf("the record says %q; want %q", lines, agent.OutcomeNotCame)
	}
}

// called is the outcomes of r's session's Called events.
func (f *fixture) called(r run.Run) []string {
	f.t.Helper()
	var out []string
	_, events, err := f.store.ReadRun(ctx, "t1", r.Session, r.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind != session.Called {
			continue
		}
		var b agent.CalledBody
		_ = json.Unmarshal(e.Body, &b)
		out = append(out, b.Outcome)
	}
	return out
}

// The wait's deadline is the connection's timeout, within a minute and
// three days; a pending answer that is also held, refused or an error
// never waits for an event (tool/kiff decides that; the runner trusts the
// state).
func TestTheWaitIsWithinAMinuteAndThreeDays(t *testing.T) {
	for _, c := range []struct {
		s    int
		want time.Duration
	}{{0, run.WaitMin}, {30, run.WaitMin}, {900, 15 * time.Minute}, {4 * 24 * 3600, run.WaitMax}} {
		if got := run.WaitFor(c.s); got != c.want {
			t.Errorf("WaitFor(%d) = %v; want %v", c.s, got, c.want)
		}
	}
}
