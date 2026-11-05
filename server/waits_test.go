package server_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
)

// waitingOnEvent is a run of ana's, paused on a sent call whose outcome
// comes later: the desk's new-ticket event, keyed by the reply's id.
func (f *runFixture) waitingOnEvent() run.Run {
	f.t.Helper()
	ana := run.Requester{Kind: run.ByMember, ID: "ana"}
	if err := f.store.Create(ctx, session.Session{ID: "s-wait", Tenant: "t1", Agent: "payouts", Kind: session.KindChat, Started: now, Updated: now, Owner: ana.Key()}); err != nil {
		f.t.Fatal(err)
	}
	r := run.New("t1", "payouts", "s-wait", run.ByOwner, "Reply to the ticket", now)
	r.Status, r.Paused = run.Paused, &run.Turn{}
	r.Requester = ana
	r.Requirements = []run.Requirement{{CallID: "c1", Tool: "desk_reply_ticket", Input: json.RawMessage(`{"ticket":"tk_1","text":"hello"}`), Kind: run.KindEvent, Expires: now.Add(15 * 60e9),
		Wait: &run.Wait{Grant: "grt_desk", Event: "DESK_NEW_TICKET", Key: "reply_1", Words: "The desk answers when the customer replies."}}}
	if err := f.runs.Create(ctx, r); err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.Begin(ctx, "t1", "s-wait", r.ID, now); err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.Hold(ctx, "t1", "s-wait", r.ID); err != nil {
		f.t.Fatal(err)
	}
	return r
}

// A wait is answered with the event's payload, by the service that matched
// the event to the run (tafs cloud), under the call's id: recorded on the
// run, and the run goes on. Another tenant, a call the run doesn't wait on,
// a body that isn't the event, and a second event are refused.
func TestAWaitIsAnsweredWithTheEventsPayload(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	r := f.waitingOnEvent()
	path := "/agents/payouts/runs/" + r.ID + "/waits/c1/answer"
	payload := map[string]any{"ticket": map[string]any{"id": "tk_1", "reply": "reply_1", "text": "thanks"}}
	code, body := f.do("t2", "POST", path, map[string]any{"payload": payload, "background": true})
	f.want(404, code, body, "another tenant")
	code, body = f.do("t1", "POST", path, map[string]any{"background": true})
	f.want(400, code, body, "no payload")
	code, body = f.do("t1", "POST", path, map[string]any{"payload": payload, "requester": map[string]any{"kind": "member", "id": "ana"}})
	f.want(400, code, body, "a requester: an event has none")
	code, body = f.do("t1", "POST", "/agents/payouts/runs/"+r.ID+"/waits/other/answer", map[string]any{"payload": payload, "background": true})
	f.want(409, code, body, "a call the run doesn't wait on")
	code, body = f.do("t1", "POST", "/agents/payouts/runs/"+r.ID+"/cards/c1/answer", map[string]any{"answer": "x", "answered_by": "tafs cloud"})
	f.want(409, code, body, "the event as a card's answer")

	code, body = f.do("t1", "POST", path, map[string]any{"payload": payload, "background": true})
	f.want(202, code, body, "the event")
	got, _ := f.runs.Get(ctx, "t1", r.ID)
	raw, _ := json.Marshal(payload)
	if q := got.Requirements[0]; q.Answered.IsZero() || string(q.Result) != string(raw) {
		t.Fatalf("recorded %+v", q)
	}
	if len(f.background) != 1 || !strings.HasSuffix(f.background[0], r.ID) {
		t.Fatalf("went on in the background: %v", f.background)
	}
	code, body = f.do("t1", "POST", path, map[string]any{"payload": payload, "background": true})
	f.want(409, code, body, "a second event")
}

// A run read while it waits says what it waits for: the event, the key,
// the deadline, and the connection's words for the owner.
func TestARunReadSaysWhatItWaitsFor(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	r := f.waitingOnEvent()
	code, body := f.do("t1", "GET", "/agents/payouts/runs/"+r.ID+"?as=service", nil)
	f.want(200, code, body, "the run")
	reqs, _ := body["requirements"].([]any)
	if len(reqs) != 1 {
		t.Fatalf("the run read: %v", body)
	}
	q, _ := reqs[0].(map[string]any)
	wait, _ := q["wait"].(map[string]any)
	if q["kind"] != "event" || wait["event"] != "DESK_NEW_TICKET" || wait["key"] != "reply_1" || wait["grant"] != "grt_desk" || wait["words"] == "" || q["expires"] == nil {
		t.Fatalf("the run read: %v", body)
	}
}
