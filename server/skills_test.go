package server_test

import (
	"testing"

	"github.com/kiff/agents/skill"
)

const kiffMD = "---\nname: kiff\ndescription: What KIFF's results mean.\n---\n\n# KIFF\n"

// A skill version is stored once, per tenant, read back whole, and listed
// without its text; an agent may name only a version its tenant stored
// (#101).
func TestSkillsAreStoredOnceAndNamedOnlyWhenThere(t *testing.T) {
	f := newFixture(t)
	body := map[string]any{"skill_md": kiffMD, "files": map[string]string{"reference/results.md": "held: wait"}}

	code, out := f.do("t1", "PUT", "/skills/kiff/abc123", body)
	f.want(200, code, out, "put")
	if out["name"] != "kiff" || out["version"] != "abc123" || out["description"] != "What KIFF's results mean." {
		t.Fatalf("put answered %v", out)
	}
	code, out = f.do("t1", "PUT", "/skills/kiff/abc123", body)
	f.want(200, code, out, "the same content again")
	code, out = f.do("t1", "PUT", "/skills/kiff/abc123", map[string]any{"skill_md": kiffMD + "\nchanged\n"})
	f.want(409, code, out, "other content under that version")

	code, out = f.do("t1", "GET", "/skills/kiff/abc123", nil)
	f.want(200, code, out, "get")
	if out["body"] != "# KIFF\n" || out["files"].(map[string]any)["reference/results.md"] != "held: wait" {
		t.Fatalf("got %v", out)
	}
	code, out = f.do("t2", "GET", "/skills/kiff/abc123", nil)
	f.want(404, code, out, "another tenant's skill")
	code, out = f.do("t1", "GET", "/skills", nil)
	f.want(200, code, out, "list")
	if list := out["skills"].([]any); len(list) != 1 || list[0].(map[string]any)["body"] != nil {
		t.Fatalf("listed %v", list)
	}

	for path, b := range map[string]any{
		"/skills/refunds/v1": map[string]any{"skill_md": kiffMD},             // the SKILL.md names kiff
		"/skills/kiff/v2":    map[string]any{"skill_md": "# no frontmatter"}, // not a SKILL.md
		"/skills/kiff/v3":    map[string]any{"skill_md": kiffMD, "files": map[string]string{"../x": "x"}},
	} {
		code, out := f.do("t1", "PUT", path, b)
		f.want(400, code, out, "PUT "+path)
	}

	d := agentBody("payouts")
	d.Skills = []skill.Ref{{Name: "kiff", Version: "nope"}}
	code, out = f.do("t1", "PUT", "/agents/payouts", d)
	f.want(400, code, out, "an agent naming a version that isn't stored")
	d.Skills = []skill.Ref{{Name: "kiff", Version: "abc123"}}
	code, out = f.do("t2", "PUT", "/agents/payouts", d)
	f.want(400, code, out, "an agent naming another tenant's skill")
	code, out = f.do("t1", "PUT", "/agents/payouts", d)
	f.want(200, code, out, "an agent naming a stored version")
}
