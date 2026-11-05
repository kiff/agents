package server_test

import (
	"strings"
	"testing"

	"github.com/kiff/agents/skill"
)

// coachWithSkills is the tenant's agent "payouts" naming two skills, cards
// then wakes, both in the skills store.
func (f *fixture) coachWithSkills(tenant string) {
	f.t.Helper()
	for _, name := range []string{"cards", "wakes"} {
		if err := f.skills.Put(ctx, skill.Package{Tenant: tenant, Name: name, Version: "1", Description: "how to " + name, Body: "The " + name + " procedure."}); err != nil {
			f.t.Fatal(err)
		}
	}
	d := agentBody("payouts")
	d.Skills = []skill.Ref{{Name: "cards", Version: "1"}, {Name: "wakes", Version: "1"}}
	code, body := f.do(tenant, "PUT", "/agents/payouts", d)
	f.want(200, code, body, "put payouts with skills")
}

// A run names the skills its session loads, each one the definition names
// by name and version; the service adds one mid-conversation, once, to the
// agent's own session only.
func TestASessionsSkillsAreTheDefinitionsAndAddedOnce(t *testing.T) {
	f := newRunFixture(t)
	f.coachWithSkills("t1")
	f.coachWithSkills("t2")
	code, body := f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "x", "background": true, "skills": []string{"wakes@2"}})
	f.want(400, code, body, "a version the definition doesn't name")
	if body["error"] != "skill_not_named" {
		t.Fatalf("the refusal: %v", body)
	}
	code, body = f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "x", "background": true, "skills": []string{"other@1"}})
	f.want(400, code, body, "a name the definition doesn't name")
	code, body = f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "x", "background": true, "skills": []string{"wakes@1"}})
	f.want(202, code, body, "a run with a skill")
	sid, _ := body["session"].(string)
	if sid == "" {
		t.Fatalf("no session: %v", body)
	}
	path := "/agents/payouts/sessions/" + sid + "/skills"
	code, body = f.do("t1", "PUT", path, map[string]any{"add": []string{"cards@1"}})
	f.want(400, code, body, "not as the service")
	code, body = f.do("t1", "PUT", path+"?as=service", map[string]any{"add": []string{"cards@2"}})
	f.want(400, code, body, "a version the definition doesn't name")
	code, body = f.do("t1", "PUT", path+"?as=service", map[string]any{"add": []string{}})
	f.want(400, code, body, "nothing to add")
	code, body = f.do("t2", "PUT", path+"?as=service", map[string]any{"add": []string{"cards@1"}})
	f.want(404, code, body, "another tenant's session")
	f.putAgent("t1", "other")
	code, body = f.do("t1", "PUT", "/agents/other/sessions/"+sid+"/skills?as=service", map[string]any{"add": []string{"cards@1"}})
	f.want(404, code, body, "another agent's session")
	code, body = f.do("t1", "PUT", path+"?as=service", map[string]any{"add": []string{"cards@1"}})
	f.want(200, code, body, "the service adds a skill")
	if got, _ := body["skills"].([]any); len(got) != 2 || got[0] != "wakes@1" || got[1] != "cards@1" {
		t.Fatalf("the session holds %v", body["skills"])
	}
	code, body = f.do("t1", "PUT", path+"?as=service", map[string]any{"add": []string{"cards@1", "wakes@1"}})
	f.want(200, code, body, "adding again")
	if got, _ := body["skills"].([]any); len(got) != 2 {
		t.Fatalf("added twice: %v", body["skills"])
	}
	// Nothing was added by the refused calls.
	if got, err := f.store.Skills(ctx, "t1", sid); err != nil || strings.Join([]string{got[0], got[1]}, " ") != "wakes@1 cards@1" || len(got) != 2 {
		t.Fatalf("the session's skills: %v, %v", got, err)
	}
}
