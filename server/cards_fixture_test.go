package server_test

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
)

// cardAnswers is testdata/card-answers.json: the requests tafs cloud sends
// for a card's answer, from the app's old form, and the result each is
// recorded as. kiff/tafs keeps the same file, byte for byte, as
// runtime/test/fixtures/card-answers.json and checks that it sends these
// bodies; a change to one is a change to both.
type cardAnswers struct {
	Cases []struct {
		Text   string          `json:"text"`
		Card   json.RawMessage `json:"card"`
		Body   json.RawMessage `json:"body"`
		Result json.RawMessage `json:"result"`
	} `json:"cases"`
}

// Each answer tafs cloud sends for a card is taken as the card's result,
// typed, never as text: the contract between the two, from one fixture.
func TestTafsCloudsCardAnswersAreTakenAsTyped(t *testing.T) {
	raw, err := os.ReadFile("testdata/card-answers.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx cardAnswers
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range fx.Cases {
		t.Run(c.Text, func(t *testing.T) {
			var card struct{ ID string }
			var body struct{ Requester run.Requester }
			if err := json.Unmarshal(c.Card, &card); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(c.Body, &body); err != nil {
				t.Fatal(err)
			}
			f := newRunFixture(t)
			f.putAgent("t1", "payouts")
			r := f.waitingOn(c.Card, body.Requester)
			code, got := f.do("t1", "POST", "/agents/payouts/runs/"+r.ID+"/cards/"+card.ID+"/answer", string(c.Body))
			f.want(202, code, got, "tafs cloud's answer")
			stored, _ := f.runs.Get(ctx, "t1", r.ID)
			if !sameJSON(stored.Requirements[0].Result, c.Result) {
				t.Fatalf("recorded %s, want %s", stored.Requirements[0].Result, c.Result)
			}
		})
	}
}

// waitingOn is a run of who's, paused on card, its session holding it.
func (f *runFixture) waitingOn(card json.RawMessage, who run.Requester) run.Run {
	f.t.Helper()
	if err := f.store.Create(ctx, session.Session{ID: "s-card", Tenant: "t1", Agent: "payouts", Kind: session.KindChat, Started: now, Updated: now, Owner: who.Key()}); err != nil {
		f.t.Fatal(err)
	}
	r := run.New("t1", "payouts", "s-card", run.ByOwner, "Connect Stripe", now)
	r.Status, r.Paused = run.Paused, &run.Turn{}
	r.Requester = who
	r.Requirements = []run.Requirement{{CallID: "s1", Tool: "show", Input: card, Kind: run.KindCard, Expires: now.Add(run.CardWait)}}
	if err := f.runs.Create(ctx, r); err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.Begin(ctx, "t1", "s-card", r.ID, now); err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.Hold(ctx, "t1", "s-card", r.ID); err != nil {
		f.t.Fatal(err)
	}
	return r
}

func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}
