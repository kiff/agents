package server_test

import (
	"testing"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
)

// A person writes only into their own: a run they asked for, a session that
// is theirs, a trial they opened. A run or session no one asked for, a
// job's, stays the service's to write into; a person may still stop it.
func TestAPersonWritesOnlyIntoTheirOwn(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	ana := map[string]any{"kind": "member", "id": "ana"}

	job := run.New("t1", "payouts", "s-job", run.BySchedule, "reconcile", now)
	job.Status = run.Running
	if err := f.runs.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	code, body := f.do("t1", "POST", "/agents/payouts/runs/"+job.ID+"/messages", map[string]any{"text": "and order 7", "requester": ana})
	f.want(404, code, body, "a person writing into a job's run")
	code, body = f.do("t1", "POST", "/agents/payouts/runs/"+job.ID+"/messages", map[string]any{"text": "and order 7"})
	f.want(202, code, body, "the service writing into a job's run")
	code, body = f.do("t1", "POST", "/agents/payouts/runs/"+job.ID+"/cancel", map[string]any{"requester": ana})
	f.want(200, code, body, "a person stopping a job's run")

	rec, err := session.NewRecorder(f.store, session.DefaultPolicy(0))
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := rec.Open(ctx, "t1", "payouts", session.KindRun)
	if err != nil {
		t.Fatal(err)
	}
	code, body = f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "hi", "background": true, "session_id": jobs.ID, "requester": ana})
	f.want(404, code, body, "a person going on in a job's session")

	f.putVersionTwo()
	code, body = f.do("t1", "PUT", "/agents/payouts/trial", map[string]any{"n": 2, "draft": "drf_t2", "requester": ana})
	f.want(200, code, body, "ana opens a trial")
	sess, _ := body["trial"].(map[string]any)["session"].(string)
	got, _, err := f.store.Read(ctx, "t1", sess)
	if err != nil || got.Owner != "member:ana" {
		t.Fatalf("the trial's chat = %+v, %v; want ana's", got, err)
	}
}

func TestOnlyItsOwnerGoesOnInASession(t *testing.T) {
	for _, c := range []struct {
		s      session.Session
		owner  string
		read   bool
		goesOn bool
	}{
		{session.Session{Kind: session.KindChat, Owner: "member:ana"}, "member:ana", true, true},
		{session.Session{Kind: session.KindChat, Owner: "member:ana"}, "member:bo", false, false},
		{session.Session{Kind: session.KindRun}, "member:ana", true, false},
		{session.Session{Kind: session.KindTrial}, "member:ana", true, false},
		{session.Session{Kind: session.KindTrial, Owner: "member:ana"}, "member:bo", false, false},
		{session.Session{Kind: session.KindRun}, "", true, true},
		{session.Session{Kind: session.KindChat}, "member:ana", false, false},
	} {
		if got := c.s.OwnedBy(c.owner); got != c.read {
			t.Errorf("%s owned by %q: read %v, want %v", c.s.Kind, c.owner, got, c.read)
		}
		if got := c.s.ContinuedBy(c.owner); got != c.goesOn {
			t.Errorf("%s owned by %q, %q goes on: %v, want %v", c.s.Kind, c.s.Owner, c.owner, got, c.goesOn)
		}
	}
}
