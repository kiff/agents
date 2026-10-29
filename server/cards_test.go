package server_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
)

const confirmCard = `{"type":"confirm","id":"which","text":"Which account?","options":[{"value":"live","label":"Live"},{"value":"test","label":"Test mode"}]}`

// waitingOnCard is a run of ana's, paused on a confirm card, its session
// holding it.
func (f *runFixture) waitingOnCard() run.Run {
	f.t.Helper()
	ana := run.Requester{Kind: run.ByMember, ID: "ana"}
	if err := f.store.Create(ctx, session.Session{ID: "s-card", Tenant: "t1", Agent: "payouts", Kind: session.KindChat, Started: now, Updated: now, Owner: ana.Key()}); err != nil {
		f.t.Fatal(err)
	}
	r := run.New("t1", "payouts", "s-card", run.ByOwner, "Connect Stripe", now)
	r.Status, r.Paused = run.Paused, &run.Turn{}
	r.Requester = ana
	r.Requirements = []run.Requirement{{CallID: "s1", Tool: "show", Input: json.RawMessage(confirmCard), Kind: run.KindCard, Expires: now.Add(run.CardWait)}}
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

// A card is answered as data, by the person whose run it is: checked
// against the card, recorded on the run, and the run goes on. Anyone else,
// a door, or an answer the card doesn't take is refused, and a card is
// answered once.
func TestACardIsAnsweredByItsOwnerAsData(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	r := f.waitingOnCard()
	path := "/agents/payouts/runs/" + r.ID + "/cards/which/answer"
	as := func(kind, id string, answer any) map[string]any {
		return map[string]any{"answer": answer, "background": true, "requester": map[string]any{"kind": kind, "id": id}}
	}
	code, body := f.do("t1", "POST", path, as("member", "luis", "test"))
	f.want(404, code, body, "another person's run")
	code, body = f.do("t1", "POST", path, as("door", "email:x@y.test", "test"))
	f.want(400, code, body, "a door's message")
	code, body = f.do("t1", "POST", path, as("member", "ana", "staging"))
	f.want(400, code, body, "an answer the card doesn't take")
	code, body = f.do("t1", "POST", "/agents/payouts/runs/"+r.ID+"/cards/other/answer", as("member", "ana", "test"))
	f.want(409, code, body, "a card the run doesn't wait on")

	code, body = f.do("t1", "POST", path, as("member", "ana", "test"))
	f.want(202, code, body, "ana's answer")
	got, _ := f.runs.Get(ctx, "t1", r.ID)
	if q := got.Requirements[0]; q.Answered.IsZero() || string(q.Result) != `{"answer":"test","words":"Test mode"}` {
		t.Fatalf("recorded %+v", q)
	}
	if len(f.background) != 1 || !strings.HasSuffix(f.background[0], r.ID) {
		t.Fatalf("went on in the background: %v", f.background)
	}
	code, body = f.do("t1", "POST", path, as("member", "ana", "live"))
	f.want(409, code, body, "a second answer")
}

// The owner writing while a card of theirs waits answers it in their words,
// and that run goes on: no second run starts beside it. A door's message
// starts a run as before, and answers nothing.
func TestWritingWhileACardWaitsAnswersIt(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	r := f.waitingOnCard()
	msg := func(kind, id string) map[string]any {
		return map[string]any{"input": "Use the live account, please", "session_id": "s-card", "background": true,
			"requester": map[string]any{"kind": kind, "id": id}}
	}
	// Whatever a door's message does (here, refused: it has no grant to this
	// chat), it never answers the card.
	code, body := f.do("t1", "POST", "/agents/payouts/runs", msg("door", "email:x@y.test"))
	if code == 202 && body["id"] == r.ID {
		t.Fatalf("a door's message answered the card: %v", body)
	}
	if got, _ := f.runs.Get(ctx, "t1", r.ID); !got.Requirements[0].Answered.IsZero() {
		t.Fatalf("a door's message answered the card: %+v", got.Requirements[0])
	}

	code, body = f.do("t1", "POST", "/agents/payouts/runs", msg("member", "ana"))
	f.want(202, code, body, "ana writes instead")
	if body["id"] != r.ID {
		t.Fatalf("ana's message started run %v; want the waiting run %s to go on", body["id"], r.ID)
	}
	got, _ := f.runs.Get(ctx, "t1", r.ID)
	var res struct {
		Answer json.RawMessage `json:"answer"`
		Words  string          `json:"words"`
		Wrote  string          `json:"wrote"`
	}
	if err := json.Unmarshal(got.Requirements[0].Result, &res); err != nil || res.Wrote != "Use the live account, please" || string(res.Answer) != "null" {
		t.Fatalf("recorded %s", got.Requirements[0].Result)
	}
}
