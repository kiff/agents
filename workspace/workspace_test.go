package workspace_test

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kiff/agents/workspace"
)

func TestCleanPath(t *testing.T) {
	for in, want := range map[string]string{
		"notes.md":         "notes.md",
		"relay/main.tf":    "relay/main.tf",
		"./a//b/../c.txt":  "a/c.txt",
		"a/./b":            "a/b",
		"dir/":             "dir",
		"a/b/../../c":      "c",
		"ünïcode/fïle.txt": "ünïcode/fïle.txt",
	} {
		got, err := workspace.CleanPath(in)
		if err != nil || got != want {
			t.Errorf("CleanPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "/etc/passwd", "..", "../x", "a/../../x", ".", "a\\b", "a\x00b", "a\nb", strings.Repeat("a", workspace.MaxPath+1)} {
		if _, err := workspace.CleanPath(in); !errors.Is(err, workspace.ErrPath) {
			t.Errorf("CleanPath(%q): %v, want ErrPath", in, err)
		}
	}
}

func TestKeyNamesATenantAndARun(t *testing.T) {
	if err := (workspace.Key{Tenant: "t1", Run: "r1"}).Check(); err != nil {
		t.Fatal(err)
	}
	for _, k := range []workspace.Key{{}, {Tenant: "t1"}, {Run: "r1"}} {
		if k.Check() == nil {
			t.Errorf("%+v passed Check", k)
		}
	}
}

func TestCutKeepsWholeCharacters(t *testing.T) {
	short := "ok"
	if s, cut := workspace.Cut(short); s != short || cut {
		t.Fatalf("Cut(short) = %q, %v", s, cut)
	}
	long := strings.Repeat("a", workspace.MaxOutput-1) + "€€"
	s, cut := workspace.Cut(long)
	if !cut || len(s) > workspace.MaxOutput || !utf8.ValidString(s) {
		t.Fatalf("Cut(long) = %d bytes, cut %v, valid %v", len(s), cut, utf8.ValidString(s))
	}
}
