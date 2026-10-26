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
	code, body = f.do("t1", "POST", "/agents/payouts/runs/"+job.ID+"/messages?as=service", map[string]any{"text": "and order 7"})
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

// A person's content is read with the person named, or as the service by
// name: naming no one is a bad request, never everyone, and naming both is
// one too. A person reads only their own runs.
func TestAReadNamesThePersonOrTheService(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.session("t1", "s-ana", "payouts")
	r := run.New("t1", "payouts", "s-ana", run.ByOwner, "x", now)
	r.Requester = run.Requester{Kind: run.ByMember, ID: "ana"}
	r.Status = run.Completed
	if err := f.runs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	runPath := "/agents/payouts/runs/" + r.ID

	for name, c := range map[string]struct{ method, path string }{
		"a run":              {"GET", runPath},
		"the runs":           {"GET", "/agents/payouts/runs"},
		"a run's stream":     {"POST", runPath + "/resume"},
		"the sessions":       {"GET", "/sessions?agent_id=payouts"},
		"a session":          {"GET", "/sessions/s-ana"},
		"the notes":          {"GET", "/agents/payouts/memories"},
		"a steer":            {"POST", runPath + "/messages"},
		"both, on a run":     {"GET", runPath + "?as=service&requester=member:ana"},
		"both, on sessions":  {"GET", "/sessions?agent_id=payouts&as=service&owner=member:ana"},
		"another as":         {"GET", runPath + "?as=admin"},
		"a forged requester": {"GET", runPath + "?requester=ana"},
	} {
		var body any
		if c.method == "POST" && name == "a steer" {
			body = map[string]any{"text": "x"}
		}
		code, got := f.do("t1", c.method, c.path, body)
		f.want(400, code, got, name)
	}

	code, got := f.do("t1", "GET", runPath+"?requester=member:ana", nil)
	f.want(200, code, got, "ana reading her own run")
	code, got = f.do("t1", "GET", runPath+"?requester=member:luis", nil)
	f.want(404, code, got, "luis reading ana's run")
	code, got = f.do("t1", "POST", runPath+"/resume?requester=member:luis", nil)
	f.want(404, code, got, "luis following ana's run")
	code, got = f.do("t1", "GET", runPath+"?as=service", nil)
	f.want(200, code, got, "the service reading ana's run")

	count := func(q string) int {
		code, got := f.do("t1", "GET", "/agents/payouts/runs?"+q, nil)
		f.want(200, code, got, q)
		runs, _ := got["runs"].([]any)
		return len(runs)
	}
	if n := count("requester=member:luis"); n != 0 {
		t.Errorf("luis lists %d runs, want none of ana's", n)
	}
	if n := count("requester=member:ana"); n != 1 {
		t.Errorf("ana lists %d runs, want her one", n)
	}
}
