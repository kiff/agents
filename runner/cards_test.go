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
	"github.com/kiff/agents/runner"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool/show"
)

const whichCard = `{"type":"confirm","id":"which","text":"Which account?","options":[{"value":"live","label":"Live"},{"value":"test","label":"Test mode"}]}`

// A card the owner answers makes the run wait, as a hold does; its answer is
// the card's result when the run goes on, never text in the chat; and the
// chat's next run reads it as the runner's line, not the owner's words.
func TestACardWaitsForItsAnswerAsData(t *testing.T) {
	f := newFixture(t, use("s1", "show", whichCard), say("Pick one on the card."), say("Test mode it is."), say("Hello."))
	f.extra = append(f.extra, show.New())
	r := f.start(server.Start{Input: "Connect my Stripe."})
	got, err := f.r.Execute(ctx, "t1", r.ID, nil)
	if err != nil || got.Status != run.Paused || len(got.Requirements) != 1 {
		t.Fatalf("ran to %s with %+v, %v; want paused on the card", got.Status, got.Requirements, err)
	}
	q := got.Requirements[0]
	if !q.Card() || q.Exception != "" || !q.Expires.Equal(f.clock.now().Add(run.CardWait)) {
		t.Fatalf("waits on %+v; want a card for run.CardWait", q)
	}
	reqs := f.model.requests()
	if note := texts(reqs[len(reqs)-1].Messages); !slices.Contains(note, "user: "+agent.CardPauseNote) {
		t.Fatalf("the waiting turn was told %q", note)
	}

	answered, _ := show.Answer(show.Component{Type: "confirm", ID: "which", Options: []show.Option{{Value: "live", Label: "Live"}, {Value: "test", Label: "Test mode"}}}, json.RawMessage(`"test"`))
	result, _ := json.Marshal(answered)
	if _, err := run.AnswerCard(ctx, f.runs, "t1", r.ID, q.CallID, result, f.clock.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := run.AnswerCard(ctx, f.runs, "t1", r.ID, q.CallID, result, f.clock.now()); err != run.ErrNotOpen {
		t.Fatalf("a second answer: %v; want ErrNotOpen", err)
	}
	if got, err = f.r.Execute(ctx, "t1", r.ID, nil); err != nil || got.Status != run.Completed {
		t.Fatalf("went on to %s, %v", got.Status, err)
	}
	reqs = f.model.requests()
	last := reqs[len(reqs)-1].Messages
	res := last[len(last)-1].Blocks[0]
	if res.Kind != model.ToolResult || res.CallID != "s1" || res.Result != `{"answer":"test","words":"Test mode"}` {
		t.Fatalf("the card's result was %+v", res)
	}
	for _, m := range last {
		for _, b := range m.Blocks {
			if m.Role == model.User && b.Kind == model.Text && strings.Contains(b.Text, "[confirm which]") {
				t.Fatalf("the answer reached the model as text: %q", b.Text)
			}
		}
	}
	if kinds := f.events(got); !slices.Contains(kinds, session.Answered) {
		t.Fatalf("recorded %v; want the answer", kinds)
	}

	// The chat's next message reads it as the runner's line.
	next := f.start(server.Start{Session: r.Session, Input: "hi"})
	if _, err := f.r.Execute(ctx, "t1", next.ID, nil); err != nil {
		t.Fatal(err)
	}
	reqs = f.model.requests()
	if h := texts(reqs[len(reqs)-1].Messages); !slices.Contains(h, "assistant: "+agent.AnsweredMark+" confirm which: Test mode") {
		t.Fatalf("the next run read %q", h)
	}
}

// A card nobody answers stops waiting after run.CardWait: the run goes on
// with no answer, and never assumes one.
func TestAnUnansweredCardStopsWaiting(t *testing.T) {
	f := newFixture(t, use("s1", "show", whichCard), say("Pick one on the card."), say("I'll ask again later."))
	f.extra = append(f.extra, show.New())
	r := f.start(server.Start{Input: "Connect my Stripe."})
	if got, _ := f.r.Execute(ctx, "t1", r.ID, nil); got.Status != run.Paused {
		t.Fatalf("ran to %s", got.Status)
	}
	if _, err := f.r.Execute(ctx, "t1", r.ID, nil); err == nil {
		t.Fatal("a card still waiting was continued")
	}
	f.clock.add(run.CardWait + time.Second)
	got, err := f.r.Execute(ctx, "t1", r.ID, nil)
	if err != nil || got.Status != run.Completed {
		t.Fatalf("went on to %s, %v", got.Status, err)
	}
	reqs := f.model.requests()
	last := reqs[len(reqs)-1].Messages
	if res := last[len(last)-1].Blocks[0]; res.Result != runner.NoAnswer {
		t.Fatalf("the card's result was %q", res.Result)
	}
}

// A run that goes on after a card's answer and then acts, as Setup does after
// its result is answered: the acting turn is stored under the continued
// run's claim, and the run finishes (Eval 40998275223).
func TestARunActsAfterACardsAnswerAndFinishes(t *testing.T) {
	f := newFixture(t, use("s1", "show", whichCard), say("Pick one on the card."), use("c2", "stripe_refund", `{"charge":"ch_1"}`), say("Refunded."))
	f.extra = append(f.extra, show.New())
	r := f.start(server.Start{Input: "Refund ch_1."})
	got, err := f.r.Execute(ctx, "t1", r.ID, nil)
	if err != nil || got.Status != run.Paused {
		t.Fatalf("ran to %s, %v", got.Status, err)
	}
	answered, _ := show.Answer(show.Component{Type: "confirm", ID: "which", Options: []show.Option{{Value: "live", Label: "Live"}, {Value: "test", Label: "Test mode"}}}, json.RawMessage(`"test"`))
	answered.AnsweredBy = "tafs cloud"
	result, _ := json.Marshal(answered)
	if _, err := run.AnswerCard(ctx, f.runs, "t1", r.ID, got.Requirements[0].CallID, result, f.clock.now()); err != nil {
		t.Fatal(err)
	}
	var acting *run.Turn
	f.refund.onCall = func() { acting = f.get(r.ID).Acting }
	got, err = f.r.Execute(ctx, "t1", r.ID, nil)
	if err != nil || got.Status != run.Completed || got.Output != "Refunded." {
		t.Fatalf("went on to %s (%q, waits on %+v), %v", got.Status, got.Output, got.Requirements, err)
	}
	if acting == nil || len(got.Done) != 2 || got.Done[1].CallID != "c2" {
		t.Fatalf("acting %v, done %+v", acting, got.Done)
	}
}
