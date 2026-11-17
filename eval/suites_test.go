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
		// The apps' words and tafs cloud's tests: nothing deployed reads either, so they touch no suite
		// (an Eval's carry was lost to app/strings/*/error.json and runtime/test/message-id.test.ts).
		{"kiff/tafs", []string{"app/strings/en/error.json", "runtime/test/message-id.test.ts"}, nil},
		// The runtime beside its tests still touches them all.
		{"kiff/tafs", []string{"runtime/test/home.test.ts", "runtime/src/cloud/home.ts"}, []string{All}},
		// tafs cloud's runtime, which every setup calls live: eval-carry.sh diffs the whole tree.
		{"kiff/tafs", []string{"runtime/src/cloud/approval.ts"}, []string{All}},
		{"kiff/tafs", []string{"runtime/skills/coach/lock.json"}, []string{"connect", "route", "setup"}},
		// The watch's skill is the route suite's alone; the agents' e2e runs it too.
		{"kiff/tafs", []string{"runtime/skills/coach/ask-a-taf/1/SKILL.md"}, []string{"route"}},
		{"kiff/agents", []string{"cmd/e2e/route.go"}, []string{"route"}},
		{"kiff/agents", []string{"cmd/e2e/planwords.go"}, []string{"connect", "route", "setup"}},
		// The tickets recipe's taf calls through the connection server at run time, not only at setup.
		{"kiff/planque", []string{"internal/gateway/proxy.go"}, []string{"connect", "recipe-tickets", "setup"}},
		// eval-carry.sh's stand-in for "a planque commit since": any path maps the same.
		{"kiff/planque", []string{"main"}, []string{"connect", "recipe-tickets", "setup"}},
		{"kiff/elsewhere", []string{"main.go"}, []string{All}},
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

// A run of some suites counts only when every other suite's newest earlier
// measure passed and nothing since touched it: Eval 19's intros failure stays
// red until connect is run again, and a change to a suite sends it back.
func TestARunOfSomeSuitesCarriesOnlyUntouchedPasses(t *testing.T) {
	s, err := LoadSuites()
	if err != nil {
		t.Fatal(err)
	}
	full := func(id int64, connect string, touched ...string) EarlierRun {
		steps := map[string]string{}
		for _, n := range s.Names() {
			for _, step := range s.Suites[n].Steps {
				steps[step] = "success"
			}
		}
		steps["Replay, introductions from a list"] = connect
		return EarlierRun{ID: id, Steps: steps, Touched: touched}
	}
	skipped := EarlierRun{ID: 30, Steps: map[string]string{"Chargebacks, two seeded runs": "skipped"}}

	// Eval 19: everything held but intros; a run of connect now carries the rest.
	from, short := s.Carry([]string{"connect"}, []EarlierRun{skipped, full(19, "failure")})
	if len(short) != 0 || from["cards"] != 19 || from["setup"] != 19 || len(from) != 5 {
		t.Errorf("connect after Eval 19: from %v, short %v", from, short)
	}
	// A run of setup alone can't carry connect: it failed there.
	if _, short := s.Carry([]string{"setup"}, []EarlierRun{full(19, "failure")}); short["connect"] != "it failed in run 19" {
		t.Errorf("connect carried from a failure: %v", short)
	}
	// A change since that touches setup sends setup back; one that touches all sends them all.
	if _, short := s.Carry([]string{"connect"}, []EarlierRun{full(19, "failure", "connect", "setup")}); short["setup"] == "" || short["cards"] != "" {
		t.Errorf("setup changed since: %v", short)
	}
	if from, _ := s.Carry([]string{"connect"}, []EarlierRun{full(19, "failure", All)}); len(from) != 0 {
		t.Errorf("carried across a change to everything: %v", from)
	}
	// The newest run that measured a suite decides it: an older pass doesn't
	// outvote a newer failure, and no earlier run is no verdict.
	partial := EarlierRun{ID: 20, Steps: map[string]string{"Hand-offs between tafs, a sender and a receiver": "failure"}}
	if _, short := s.Carry([]string{"connect"}, []EarlierRun{partial, full(19, "success")}); short["handoffs"] != "it failed in run 20" {
		t.Errorf("an older pass outvoted a newer failure: %v", short)
	}
	if _, short := s.Carry([]string{"connect"}, nil); len(short) != 5 {
		t.Errorf("no earlier run: %v", short)
	}
}
