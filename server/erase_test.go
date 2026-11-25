package server_test

import (
	"encoding/json"
	"testing"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
)

// A person deleting their own thread deletes what was written in its runs too:
// what they asked and wrote, what it answered, the calls' inputs and results,
// and its turns. What stays is the record that each ran and what it cost, so
// the account's history and billing hold. Another thread's run keeps its words,
// and a thread whose run is still going is refused, with nothing erased.
func TestDeletingAThreadErasesItsRuns(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	rec, err := session.NewRecorder(f.store, session.DefaultPolicy(0))
	if err != nil {
		t.Fatal(err)
	}
	anas, err := rec.OpenOwned(ctx, "t1", "payouts", session.KindChat, "member:ana")
	if err != nil {
		t.Fatal(err)
	}
	other, err := rec.OpenOwned(ctx, "t1", "payouts", session.KindChat, "member:ana")
	if err != nil {
		t.Fatal(err)
	}
	words := json.RawMessage(`{"email":"ana@example.com"}`)
	mk := func(session string, status run.Status) run.Run {
		r := run.New("t1", "payouts", session, run.ByOwner, "", now)
		r.Status, r.Input, r.Aside, r.Output, r.Error = status, "refund order 7 for ana@example.com", "the page", "Refunded.", "said ana@example.com"
		r.Cost, r.Turns, r.Tokens.Input = 1234, 3, 900
		r.Steering = []run.Steering{{Text: "and order 8", At: now, Taken: true}}
		r.Requirements = []run.Requirement{{CallID: "c1", Tool: "refund", Input: words, Operation: "op1", Exception: "ex1", Answer: "approved", Result: words}}
		r.NotAgain = []run.Call{{Tool: "refund", Input: words}}
		r.Done = []run.Done{{CallID: "c1", Tool: "refund", Input: words, Invocation: "inv1"}}
		if err := f.runs.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	ended := mk(anas.ID, run.Completed)
	kept := mk(other.ID, run.Completed)
	going := mk(other.ID, run.Paused)

	code, got := f.do("t1", "DELETE", "/sessions/"+other.ID+"?owner=member:ana", nil)
	f.want(409, code, got, "deleting a thread whose run is paused")
	if r, _ := f.runs.Get(ctx, "t1", kept.ID); r.Erased || r.Input == "" {
		t.Fatalf("a refused delete erased a run: %+v", r)
	}
	_ = going

	code, got = f.do("t1", "DELETE", "/sessions/"+anas.ID+"?owner=member:ana", nil)
	f.want(204, code, got, "ana deleting her own thread")
	r, err := f.runs.Get(ctx, "t1", ended.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Erased || r.Input != "" || r.Aside != "" || r.Output != "" || r.Error != "" || r.Paused != nil || r.Acting != nil {
		t.Errorf("the run keeps what was written: %+v", r)
	}
	// A call's input reads back as JSON null once it's gone: nothing of it is kept.
	empty := func(raw json.RawMessage) bool { return len(raw) == 0 || string(raw) == "null" }
	if r.Steering[0].Text != "" || !empty(r.Requirements[0].Input) || !empty(r.Requirements[0].Result) || !empty(r.NotAgain[0].Input) || !empty(r.Done[0].Input) {
		t.Errorf("the run keeps a call's or a message's words: %+v", r)
	}
	if r.Status != run.Completed || r.Cost != 1234 || r.Turns != 3 || r.Tokens.Input != 900 || r.Requirements[0].Exception != "ex1" ||
		r.Requirements[0].Answer != "approved" || r.Done[0].Invocation != "inv1" || r.Session != anas.ID {
		t.Errorf("the run's record didn't stay: %+v", r)
	}
	if r, _ := f.runs.Get(ctx, "t1", kept.ID); r.Erased || r.Input == "" || r.Output == "" {
		t.Errorf("another thread's run was erased: %+v", r)
	}
	// Again: the thread is gone.
	code, got = f.do("t1", "DELETE", "/sessions/"+anas.ID+"?owner=member:ana", nil)
	f.want(404, code, got, "deleting it again")
}
