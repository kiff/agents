package server_test

import (
	"testing"

	"github.com/kiff/agents/skill"
)

// A run that opens a session may limit it (only): it is kept with the
// session and shown with it; a limit on a session that exists, or with a live
// chat, is refused; the service adds no skill to a session limited to its
// own; and a live message never goes on in a limited session, however recent.
func TestARunLimitsTheSessionItOpens(t *testing.T) {
	f := newRunFixture(t)
	f.coachWithSkills("t1")
	ana := map[string]any{"kind": "member", "id": "ana"}
	only := map[string]any{"skills": true, "tools": []string{"tafs_state", "taf_task"}}
	code, body := f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "x", "background": true, "chat": "live", "requester": ana, "only": only})
	f.want(400, code, body, "a limit with a live chat")
	code, body = f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "x", "background": true, "requester": ana, "only": map[string]any{"skills": true, "tools": []string{}}})
	f.want(400, code, body, "a limit naming no tool")
	code, body = f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "x", "background": true, "requester": ana, "skills": []string{"wakes@1"}, "only": only})
	f.want(202, code, body, "a limited session")
	limited, _ := body["session"].(string)
	code, body = f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "y", "background": true, "session_id": limited, "requester": ana, "only": only})
	f.want(400, code, body, "a limit on a session that exists")
	code, body = f.do("t1", "PUT", "/agents/payouts/sessions/"+limited+"/skills?as=service", map[string]any{"add": []string{"cards@1"}})
	f.want(409, code, body, "the service adding a skill to a limited session")
	if body["error"] != "skills_limited" {
		t.Fatalf("the refusal: %v", body)
	}
	code, body = f.do("t1", "GET", "/sessions/"+limited+"?owner=member:ana", nil)
	f.want(200, code, body, "ana reading her limited session")
	sess, _ := body["session"].(map[string]any)
	if o, _ := sess["only"].(map[string]any); o == nil || o["skills"] != true {
		t.Fatalf("the session shows %v", body)
	}
	// Her live message, a moment later, opens a chat of its own.
	code, body = f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "z", "background": true, "chat": "live", "requester": ana})
	f.want(202, code, body, "ana's live message")
	if body["session"] == limited || body["new_session"] != true {
		t.Fatalf("a live message went on in the limited session: %v", body)
	}
}

// A named-only skill is added by no one after a session opens: not by the
// service, not by a later run.
func TestNoOneAddsANamedOnlySkillLater(t *testing.T) {
	f := newRunFixture(t)
	if err := f.skills.Put(ctx, skill.Package{Tenant: "t1", Name: "ask", Version: "1", Description: "how to ask", Body: "The ask procedure."}); err != nil {
		t.Fatal(err)
	}
	f.coachWithSkills("t1")
	d := agentBody("payouts")
	d.Skills = []skill.Ref{{Name: "cards", Version: "1"}, {Name: "wakes", Version: "1"}, {Name: "ask", Version: "1", NamedOnly: true}}
	code, body := f.do("t1", "PUT", "/agents/payouts", d)
	f.want(200, code, body, "put payouts with a named-only skill")
	code, body = f.do("t1", "GET", "/agents/payouts", nil)
	f.want(200, code, body, "read it back")
	if skills, _ := body["skills"].([]any); len(skills) != 3 || skills[2].(map[string]any)["named_only"] != true {
		t.Fatalf("the definition's skills: %v", body["skills"])
	}
	ana := map[string]any{"kind": "member", "id": "ana"}
	code, body = f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "x", "background": true, "requester": ana, "skills": []string{"cards@1"}})
	f.want(202, code, body, "a phone chat")
	sid, _ := body["session"].(string)
	code, body = f.do("t1", "PUT", "/agents/payouts/sessions/"+sid+"/skills?as=service", map[string]any{"add": []string{"ask@1"}})
	f.want(409, code, body, "the service adding the named-only skill")
	code, body = f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "y", "background": true, "session_id": sid, "requester": ana, "skills": []string{"ask@1"}})
	f.want(409, code, body, "a later run naming it")
	// A session that opens with it holds it.
	code, body = f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "z", "background": true, "requester": ana, "skills": []string{"ask@1"}})
	f.want(202, code, body, "a session opening with it")
}
