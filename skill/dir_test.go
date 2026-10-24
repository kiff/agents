package skill_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kiff/agents/skill"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDirReadsEachVersionWithItsFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "kiff", "1", "SKILL.md"), kiffMD)
	write(t, filepath.Join(dir, "kiff", "1", "refs", "results.md"), "held: wait")
	write(t, filepath.Join(dir, "kiff", "2", "SKILL.md"), kiffMD)
	ps, err := skill.LoadDir(dir, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 {
		t.Fatalf("got %d packages, want 2", len(ps))
	}
	one := ps[0]
	if one.Tenant != "t1" || one.Name != "kiff" || one.Version != "1" || one.Files["refs/results.md"] != "held: wait" {
		t.Fatalf("got %+v", one)
	}
	if ps[1].Version != "2" || ps[1].Files != nil {
		t.Fatalf("a version with no files has none: %+v", ps[1])
	}
}

func TestLoadDirRefusesAMisnamedOrEmptyDirectory(t *testing.T) {
	if _, err := skill.LoadDir(t.TempDir(), "t1"); err == nil || !strings.Contains(err.Error(), "no <name>/<version>/SKILL.md") {
		t.Fatalf("an empty directory: %v", err)
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, "other", "1", "SKILL.md"), kiffMD)
	if _, err := skill.LoadDir(dir, "t1"); err == nil || !strings.Contains(err.Error(), "its directory must be named so") {
		t.Fatalf("a skill under another name's directory: %v", err)
	}
}
