package server_test

import (
	"fmt"
	"strings"
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

// A shared note of unknown origin is quarantined: no run reads it, a
// person's list doesn't show it, and the service's list marks it. Someone
// who manages the agent shares it on purpose (by) or forgets it.
func TestANoteOfUnknownOriginIsQuarantined(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	for _, n := range []session.Note{
		{Tenant: "t1", Agent: "payouts", Name: "legacy", Text: "old rule", Updated: now},
		{Tenant: "t1", Agent: "payouts", Name: "job", Text: "a job's", Updated: now, Run: "r1"},
	} {
		if err := f.store.PutNote(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	names := func(q string) []string {
		code, got := f.do("t1", "GET", "/agents/payouts/memories?"+q, nil)
		f.want(200, code, got, q)
		var out []string
		for _, m := range got["memories"].([]any) {
			n := m.(map[string]any)
			out = append(out, fmt.Sprint(n["name"], " ", n["quarantined"] == true))
		}
		return out
	}
	if got := names("owner=member:ana"); fmt.Sprint(got) != "[job false]" {
		t.Fatalf("ana sees %v, want only the job's note", got)
	}
	if got := names("as=service"); fmt.Sprint(got) != "[job false legacy true]" {
		t.Fatalf("the service sees %v, want legacy marked", got)
	}
	all, _ := f.store.ListNotes(ctx, "t1", "payouts")
	if got := session.NotesFor(all, "member:ana"); len(got) != 1 || got[0].Name != "job" {
		t.Fatalf("a run reads %+v", got)
	}

	for name, q := range map[string]string{"no one": "", "a door": "?by=door:x", "a bad key": "?by=ana"} {
		code, got := f.do("t1", "POST", "/agents/payouts/memories/legacy/share"+q, nil)
		f.want(400, code, got, "releasing by "+name)
	}
	code, got := f.do("t1", "POST", "/agents/payouts/memories/job/share?by=member:ana&as=service", nil)
	f.want(409, code, got, "releasing a note shared already")
	code, got = f.do("t1", "POST", "/agents/payouts/memories/legacy/share?by=member:ana&as=service", nil)
	f.want(200, code, got, "ana shares it on purpose")
	if got := names("owner=member:luis"); fmt.Sprint(got) != "[job false legacy false]" {
		t.Fatalf("after release luis sees %v", got)
	}
}

// A write that names a person never also says it acts as the service.
func TestAWriteNamesThePersonOrTheServiceNotBoth(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	ana := map[string]any{"kind": "member", "id": "ana"}
	r := f.stored(run.Running)
	code, body := f.do("t1", "POST", "/agents/payouts/runs/"+r.ID+"/messages?as=service", map[string]any{"text": "x", "requester": ana})
	f.want(400, code, body, "a steer naming ana and the service")
	code, body = f.do("t1", "POST", "/agents/payouts/runs?as=service", map[string]any{"input": "x", "background": true, "requester": ana})
	f.want(400, code, body, "a start naming ana and the service")
}

// Deleting a thread, forgetting a note and sharing one name the person, who
// acts only on their own, or say the service acts; naming no one is refused.
func TestDeletingForgettingAndSharingNameWhoActs(t *testing.T) {
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
	if err := f.store.PutNote(ctx, session.Note{Tenant: "t1", Agent: "payouts", Scope: "member:ana", Name: "tone", Text: "short", Updated: now, Run: "r1"}); err != nil {
		t.Fatal(err)
	}

	for name, c := range map[string]struct{ method, path string }{
		"deleting a thread": {"DELETE", "/sessions/" + anas.ID},
		"forgetting a note": {"DELETE", "/agents/payouts/memories/tone?scope=member:ana"},
		"sharing a note":    {"POST", "/agents/payouts/memories/tone/share?scope=member:ana"},
	} {
		code, got := f.do("t1", c.method, c.path, nil)
		f.want(400, code, got, name+" naming no one")
		sep := "?"
		if strings.Contains(c.path, "?") {
			sep = "&"
		}
		code, got = f.do("t1", c.method, c.path+sep+"owner=member:luis", nil)
		f.want(404, code, got, name+" as luis, not his")
	}

	code, got := f.do("t1", "POST", "/agents/payouts/memories/tone/share?scope=member:ana&owner=member:ana", nil)
	f.want(200, code, got, "ana sharing her own note")
	code, got = f.do("t1", "DELETE", "/agents/payouts/memories/tone?as=service", nil)
	f.want(204, code, got, "the service forgetting the shared note")
	code, got = f.do("t1", "DELETE", "/sessions/"+anas.ID+"?owner=member:ana", nil)
	f.want(204, code, got, "ana deleting her own thread")

	// A job's thread, with no owner, is shared history: only the service deletes it.
	job, err := rec.Open(ctx, "t1", "payouts", session.KindRun)
	if err != nil {
		t.Fatal(err)
	}
	code, got = f.do("t1", "DELETE", "/sessions/"+job.ID+"?owner=member:ana", nil)
	f.want(404, code, got, "ana deleting a job's thread")
	code, got = f.do("t1", "DELETE", "/sessions/"+job.ID+"?as=service", nil)
	f.want(204, code, got, "the service deleting a job's thread")
}
