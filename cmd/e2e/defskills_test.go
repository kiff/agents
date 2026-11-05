package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/skill"
	"github.com/kiff/agents/store/memory"
)

// infra/copy-skill-packages.sh copies every package of a kiff/tafs skills
// tree, at any depth, to <name>/<version>/, and leaves the kiff skill out
// (kiff/tafs#525 put Coach's skills a level down, and the Eval found none).
func TestCopySkillPackagesFindsThemAtAnyDepth(t *testing.T) {
	src, out := t.TempDir(), filepath.Join(t.TempDir(), "skills")
	write := func(rel, body string) {
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	md := func(name string) string {
		return "---\nname: " + name + "\ndescription: Does " + name + ". Use when testing.\n---\n\nBody.\n"
	}
	write("kiff/SKILL.md", md("kiff"))
	write("coach/set-up-a-taf/1/SKILL.md", md("set-up-a-taf"))
	write("coach/name-the-tools/2/SKILL.md", md("name-the-tools"))
	write("coach/name-the-tools/2/references/forms.md", "forms")
	write("top/1/SKILL.md", md("top"))
	write("coach/lock.json", "{}")
	cmd := exec.Command("sh", "../../infra/copy-skill-packages.sh", src, out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	ps, err := skill.LoadDir(out, "t1")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, p := range ps {
		got[p.Name+"@"+p.Version] = true
	}
	for _, want := range []string{"set-up-a-taf@1", "name-the-tools@2", "top@1"} {
		if !got[want] {
			t.Fatalf("%s not copied: %v", want, got)
		}
	}
	if got["kiff@SKILL.md"] || len(got) != 3 {
		t.Fatalf("copied more than the packages: %v", got)
	}
	if _, err := os.Stat(filepath.Join(out, "name-the-tools", "2", "references", "forms.md")); err != nil {
		t.Fatalf("a package's files: %v", err)
	}
	// A tree with no packages makes no directory, so the workflow passes no -skills.
	empty, none := t.TempDir(), filepath.Join(t.TempDir(), "skills")
	if err := os.WriteFile(filepath.Join(empty, "SKILL.md"), []byte(md("kiff")), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command("sh", "../../infra/copy-skill-packages.sh", empty, none).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	if _, err := os.Stat(none); !os.IsNotExist(err) {
		t.Fatalf("made %s with no packages", none)
	}
}

// With E2E_DEFS (an infra/eval-definitions.sh output), Setup's definition
// registers with every skill it names loaded, as the e2e's runtime builds it:
// the step the Eval failed at, with no model call.
func TestDefinitionsRegisterSetupWithItsSkills(t *testing.T) {
	dir := os.Getenv("E2E_DEFS")
	if dir == "" {
		t.Skip("E2E_DEFS not set: give an infra/eval-definitions.sh output")
	}
	ctx := context.Background()
	ps, err := skill.LoadDir(filepath.Join(dir, "skills"), "t1")
	if err != nil {
		t.Fatal(err)
	}
	store := memory.NewSkills()
	for _, p := range ps {
		if err := store.Put(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "setup.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d registry.Definition
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	if err := skill.CheckRefs(d.Skills); err != nil {
		t.Fatal(err)
	}
	loaded, err := registry.LoadSkills(ctx, store, "t1", d.Skills)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Setup names %d skills; %d packages stored; %d loaded, speed %q", len(d.Skills), len(ps), len(loaded), d.Speed)
	if len(loaded) != len(d.Skills) || len(d.Skills) == 0 {
		t.Fatalf("loaded %d of %d", len(loaded), len(d.Skills))
	}
}
