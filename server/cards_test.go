package server_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

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

// An approval card answered "approved" is the owner's word on the card, never
// KIFF's signed approval: a hold KIFF keeps on the same run stays held, and
// the run doesn't go on.
func TestAnApprovalCardNeverStandsInForKIFFsApproval(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	r := f.waitingOnCard()
	got, _ := f.runs.Get(ctx, "t1", r.ID)
	if _, err := run.Change(ctx, f.runs, got, now, func(n *run.Run) error {
		n.Requirements = []run.Requirement{
			{CallID: "s2", Tool: "show", Input: json.RawMessage(`{"type":"approval","id":"connect","grant":"grt_1"}`), Kind: run.KindCard, Expires: now.Add(run.CardWait)},
			{CallID: "c1", Tool: "stripe_refund", Operation: "op1", Exception: "exc_1", Expires: now.Add(time.Hour)},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	code, body := f.do("t1", "POST", "/agents/payouts/runs/"+r.ID+"/cards/connect/answer",
		map[string]any{"answer": "approved", "background": true, "requester": map[string]any{"kind": "member", "id": "ana"}})
	f.want(202, code, body, "the approval card answered")
	got, _ = f.runs.Get(ctx, "t1", r.ID)
	hold := got.Requirements[1]
	if got.Status != run.Paused || !hold.Answered.IsZero() || hold.Answer != "" || len(f.background) != 0 || len(f.runner.execs) != 0 {
		t.Fatalf("the hold moved: status %s, hold %+v, went on %v %v", got.Status, hold, f.background, f.runner.execs)
	}
	code, body = f.do("t1", "POST", "/agents/payouts/runs/"+r.ID+"/continue", nil)
	f.want(409, code, body, "the run still waits for KIFF")
}

const resultCard = `{"type":"result","id":"done","text":"Refunds is ready.","taf":{"agent":"refunds","name":"Refunds","job":"refunds","draft":"drf_1"}}`

// A result is answered by the service that hands the owner over, naming
// itself, and the run records it as that service's answer, never the
// owner's. The person can't answer it, a service can't answer the person's
// cards, and an answer can't name both.
func TestAResultIsAnsweredByTheServiceNeverAsTheOwner(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	r := f.waitingOnCard()
	got, _ := f.runs.Get(ctx, "t1", r.ID)
	if _, err := run.Change(ctx, f.runs, got, now, func(n *run.Run) error {
		n.Requirements = append(n.Requirements, run.Requirement{CallID: "s2", Tool: "show", Input: json.RawMessage(resultCard), Kind: run.KindCard, Expires: now.Add(run.CardWait)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	result := "/agents/payouts/runs/" + r.ID + "/cards/done/answer"
	ana := map[string]any{"kind": "member", "id": "ana"}
	code, body := f.do("t1", "POST", result, map[string]any{"answer": map[string]any{"taf_id": "taf_9"}, "requester": ana})
	f.want(400, code, body, "the person answering a result")
	code, body = f.do("t1", "POST", result, map[string]any{"answer": map[string]any{"taf_id": "taf_9"}, "requester": ana, "answered_by": "tafs cloud"})
	f.want(400, code, body, "both the person and a service")
	code, body = f.do("t1", "POST", result, map[string]any{"answer": map[string]any{"taf_id": "taf_9"}, "answered_by": "Tafs Cloud!"})
	f.want(400, code, body, "a service name out of shape")
	code, body = f.do("t1", "POST", "/agents/payouts/runs/"+r.ID+"/cards/which/answer", map[string]any{"answer": "test", "answered_by": "tafs cloud"})
	f.want(400, code, body, "a service answering the person's card")

	code, body = f.do("t1", "POST", result, map[string]any{"answer": map[string]any{"taf_id": "taf_9"}, "answered_by": "tafs cloud", "background": true})
	f.want(202, code, body, "tafs cloud's answer")
	got, _ = f.runs.Get(ctx, "t1", r.ID)
	if q := got.Requirements[1]; q.Answered.IsZero() || string(q.Result) != `{"answer":{"taf_id":"taf_9"},"words":"Refunds is set up","answered_by":"tafs cloud"}` {
		t.Fatalf("recorded %+v", q)
	}
	if !got.Requirements[0].Answered.IsZero() || len(f.background) != 0 {
		t.Fatalf("the person's card moved, or the run went on before it was answered: %+v %v", got.Requirements[0], f.background)
	}
	code, body = f.do("t1", "POST", result, map[string]any{"answer": map[string]any{"failed": "x"}, "answered_by": "tafs cloud"})
	f.want(409, code, body, "a second answer")
}
