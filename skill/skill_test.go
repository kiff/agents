package skill_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kiff/agents/skill"
	"github.com/kiff/agents/tool"
)

const kiffMD = "---\nname: kiff\ndescription: \"Work with KIFF: what a held, refused or unknown result means.\"\nlicense: MIT\n---\n\n# KIFF\n\nNever route around the Card.\n"

func TestParseReadsTheFrontmatterAndBody(t *testing.T) {
	name, desc, body, err := skill.Parse(kiffMD)
	if err != nil {
		t.Fatal(err)
	}
	if name != "kiff" || desc != "Work with KIFF: what a held, refused or unknown result means." || body != "# KIFF\n\nNever route around the Card.\n" {
		t.Fatalf("parsed %q, %q, %q", name, desc, body)
	}
	if _, _, _, err := skill.Parse(strings.ReplaceAll(kiffMD, "\n", "\r\n")); err != nil {
		t.Fatalf("CRLF: %v", err)
	}
	// A YAML block's text is on the lines after its indicator, which this reader
	// doesn't take: refused, rather than stored as the description ">".
	for _, block := range []string{">", "|", ">-", "|+", ">2"} {
		md := "---\nname: kiff\ndescription: " + block + "\n  Work with KIFF.\n---\nbody"
		if _, _, _, err := skill.Parse(md); err == nil || !strings.Contains(err.Error(), "write it on one line") {
			t.Errorf("description %q: %v", block, err)
		}
	}
	for _, bad := range []string{"# no frontmatter", "---\nname: kiff\n", "---\nname: kiff\n---\nbody", "---\ndescription: d\n---\nbody"} {
		if _, _, _, err := skill.Parse(bad); err == nil {
			t.Errorf("parsed %q", bad)
		}
	}
}

func TestCheckBoundsAPackage(t *testing.T) {
	ok := skill.Package{Tenant: "t1", Name: "kiff", Version: "abc123", Description: "d", Body: "b", Files: map[string]string{"reference/a.md": "a"}}
	if err := ok.Check(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*skill.Package){
		"no tenant":          func(p *skill.Package) { p.Tenant = "" },
		"an uppercase name":  func(p *skill.Package) { p.Name = "Kiff" },
		"a version with '/'": func(p *skill.Package) { p.Version = "a/b" },
		"a long description": func(p *skill.Package) { p.Description = strings.Repeat("d", skill.MaxDescription+1) },
		"an empty body":      func(p *skill.Package) { p.Body = " \n" },
		"a path out":         func(p *skill.Package) { p.Files = map[string]string{"../x": "x"} },
		"an absolute path":   func(p *skill.Package) { p.Files = map[string]string{"/etc/x": "x"} },
		"a second SKILL.md":  func(p *skill.Package) { p.Files = map[string]string{"SKILL.md": "x"} },
		"too large, stored":  func(p *skill.Package) { p.Body = strings.Repeat(`"`, skill.MaxSize/2+1) }, // '"' is two bytes stored
		"a file not UTF-8":   func(p *skill.Package) { p.Files = map[string]string{"a.bin": "\xff"} },
	} {
		p := ok
		change(&p)
		if err := p.Check(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if err := skill.CheckRefs([]skill.Ref{{Name: "kiff", Version: "v1"}, {Name: "kiff", Version: "v2"}}); err == nil {
		t.Error("one skill named twice")
	}
	if err := skill.CheckRefs(make([]skill.Ref, skill.MaxPerAgent+1)); err == nil {
		t.Error("too many skills")
	}
}

// The catalog names each skill and what it is for; the tool reads a skill's
// body or one of its files, and only the agent's own skills. It is read-only
// and off the gateway: a skill widens nothing.
func TestTheCatalogAndTheToolReadOnlyTheAgentsSkills(t *testing.T) {
	ps := []skill.Package{
		{Name: "kiff", Version: "v1", Description: "What KIFF's\n results mean.", Body: "# KIFF", Files: map[string]string{"reference/results.md": "held: wait"}},
		{Name: "refunds", Version: "v2", Description: "How to refund.", Body: "# Refunds"},
	}
	cat := skill.Catalog(ps)
	for _, want := range []string{"# Skills", "- kiff: What KIFF's results mean.", "- refunds: How to refund.", "widens no Card"} {
		if !strings.Contains(cat, want) {
			t.Errorf("catalog lacks %q:\n%s", want, cat)
		}
	}
	if skill.Catalog(nil) != "" {
		t.Error("a catalog with no skills")
	}

	tl := skill.NewTool(ps)
	if tl.Access() != tool.Read || tl.Gateway() {
		t.Fatalf("access %v, gateway %v", tl.Access(), tl.Gateway())
	}
	if err := tool.Check([]tool.Tool{tl}); err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct{ Name struct{ Enum []string } }
	}
	if err := json.Unmarshal(tl.Spec().InputSchema, &schema); err != nil || strings.Join(schema.Properties.Name.Enum, ",") != "kiff,refunds" {
		t.Fatalf("schema %s: %v", tl.Spec().InputSchema, err)
	}
	ctx := context.Background()
	for in, want := range map[string]string{
		`{"name":"kiff"}`:                                 "# KIFF",
		`{"name":"kiff","file":"SKILL.md"}`:               "# KIFF",
		`{"name":"kiff","file":"reference/results.md"}`:   "held: wait",
		`{"name":"kiff","file":"./reference/results.md"}`: "held: wait",
		`{"name":"refunds"}`:                              "# Refunds",
	} {
		r, err := tl.Call(ctx, json.RawMessage(in))
		if err != nil || r.IsError || r.Text != want {
			t.Errorf("%s: %+v, %v", in, r, err)
		}
	}
	for _, in := range []string{`{"name":"other"}`, `{"name":"kiff","file":"../secrets"}`, `not json`} {
		r, err := tl.Call(ctx, json.RawMessage(in))
		if err != nil || !r.IsError {
			t.Errorf("%s: %+v, %v; want a result the model can correct", in, r, err)
		}
	}
}
