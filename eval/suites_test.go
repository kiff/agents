package eval

import (
	"slices"
	"testing"
)

// A change runs the suites its paths touch: the first prefix decides, the
// apps and docs touch none, and a path no prefix covers, or a repository not
// listed, touches them all.
func TestAChangeRunsTheSuitesItsPathsTouch(t *testing.T) {
	s, err := LoadSuites()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		repo  string
		paths []string
		want  []string
	}{
		{"kiff/tafs", []string{"runtime/skills/coach/connect-an-app/17/SKILL.md"}, []string{"connect", "setup"}},
		{"kiff/tafs", []string{"runtime/skills/coach/the-dial/3/SKILL.md"}, []string{"setup"}},
		{"kiff/tafs", []string{"android/app/src/Main.kt", "docs/coach.md"}, nil},
		{"kiff/tafs", []string{"runtime/skills/kiff/SKILL.md", "ios/Tafs/App.swift"}, []string{"cards", "handoffs", "recipe-tickets"}},
		{"kiff/tafs", []string{"runtime/src/cloud/tafdef.ts"}, []string{All}},
		{"kiff/planque", []string{"internal/planque/apikey.go"}, []string{"connect", "setup"}},
		{"kiff/kiff-cloud", []string{"apps/api/main.go"}, []string{All}},
	} {
		if got := s.For(c.repo, c.paths); !slices.Equal(got, c.want) {
			t.Errorf("%s %v: %v, want %v", c.repo, c.paths, got, c.want)
		}
	}
	for asked, want := range map[string]string{"": ",all,", "all": ",all,", "setup, connect": ",connect,setup,", "cards,cards": ",cards,", "connect all": ",all,"} {
		if got, err := s.Asked(asked); err != nil || got != want {
			t.Errorf("asked %q: %q %v, want %q", asked, got, err, want)
		}
	}
	if _, err := s.Asked("connect, nope"); err == nil {
		t.Error("an unknown suite was taken")
	}
}
