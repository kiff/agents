package runner_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/registry/registrytest"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/skill"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
)

// withSkills gives the fixture's agent two skills, cards then wakes, and a
// store holding them.
func withSkills(t *testing.T, f *fixture) {
	t.Helper()
	skills := memory.NewSkills()
	for _, name := range []string{"cards", "wakes"} {
		if err := skills.Put(ctx, skill.Package{Tenant: "t1", Name: name, Version: "1", Description: "how to " + name, Body: "The " + name + " procedure: step one, step two."}); err != nil {
			t.Fatal(err)
		}
	}
	f.r.Builder.Skills = skills
	d := registrytest.Definition("t1", "rae")
	d.Skills = []skill.Ref{{Name: "cards", Version: "1"}, {Name: "wakes", Version: "1"}}
	if err := f.agents.Put(ctx, d); err != nil {
		t.Fatal(err)
	}
}

// A session's first run names the skills it loads: their bodies follow the
// instructions in the definition's order, never the order they were named
// in, each as "# Skill: <name>"; the next run in the session carries them
// too. A skill the definition doesn't name, by name or by version, refuses
// the run.
func TestARunsSkillsFollowTheInstructionsInTheDefinitionsOrder(t *testing.T) {
	f := newFixture(t, say("Hello."), say("Again."))
	withSkills(t, f)
	r := f.start(server.Start{Input: "hi", Skills: []string{"wakes@1", "cards@1"}})
	if _, err := f.r.Execute(ctx, "t1", r.ID, nil); err != nil {
		t.Fatal(err)
	}
	system := f.model.requests()[0].System
	cards, wakes := strings.Index(system, "# Skill: cards\nThe cards procedure"), strings.Index(system, "# Skill: wakes\nThe wakes procedure")
	if cards < 0 || wakes < 0 || cards > wakes || cards < strings.Index(system, "Skills") {
		t.Fatalf("the context: cards at %d, wakes at %d:\n%s", cards, wakes, system)
	}
	if got, _ := f.store.Skills(ctx, "t1", r.Session); strings.Join(got, " ") != "wakes@1 cards@1" {
		t.Fatalf("the session holds %v", got)
	}
	// The next run in the session, naming nothing, carries them too.
	next := f.start(server.Start{Session: r.Session, Input: "more"})
	if _, err := f.r.Execute(ctx, "t1", next.ID, nil); err != nil {
		t.Fatal(err)
	}
	if system := f.model.requests()[1].System; !strings.Contains(system, "# Skill: cards") || !strings.Contains(system, "# Skill: wakes") {
		t.Fatalf("the next run's context lost the skills:\n%s", system)
	}
	for _, bad := range [][]string{{"wakes@2"}, {"other@1"}, {"cards@1", "cards@1"}, {""}} {
		_, err := f.r.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "hi", Skills: bad})
		if !errors.Is(err, registry.ErrSkillNotNamed) {
			t.Fatalf("skills %q started: %v", bad, err)
		}
	}
	if got, _ := f.store.List(ctx, "t1", "rae", 10); len(got) != 1 {
		t.Fatalf("a refused start opened a session: %d sessions", len(got))
	}
}

// Naming skills never lets a run into a session that isn't its own: another
// person's chat, or another agent's session, is not found with skills as
// without (#219), and the session gains none (qa-kiff's probe on #312).
func TestSkillsNeverOpenAnotherPersonsOrAgentsSession(t *testing.T) {
	f := newFixture(t, say("Hello."))
	withSkills(t, f)
	ana := run.Requester{Kind: run.ByMember, ID: "ana"}
	luis := run.Requester{Kind: run.ByMember, ID: "luis"}
	r := f.start(server.Start{Input: "hi", Requester: ana})
	if _, err := f.r.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Session: r.Session, Input: "x", Requester: luis, Skills: []string{"cards@1"}}); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("luis going on in ana's session with skills: %v, want not found", err)
	}
	other := registrytest.Definition("t1", "other")
	other.Skills = []skill.Ref{{Name: "cards", Version: "1"}}
	if err := f.agents.Put(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Start(ctx, server.Start{Tenant: "t1", Agent: "other", Trigger: run.ByOwner, Session: r.Session, Input: "x", Requester: ana, Skills: []string{"cards@1"}}); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("another agent going on in rae's session with skills: %v, want not found", err)
	}
	if got, err := f.store.Skills(ctx, "t1", r.Session); err != nil || len(got) != 0 {
		t.Fatalf("the refused runs added skills: %v, %v", got, err)
	}
	// Ana herself, with skills, goes on and the session gains them.
	if next, err := f.r.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Session: r.Session, Input: "more", Requester: ana, Skills: []string{"cards@1"}}); err != nil || next.Session != r.Session {
		t.Fatalf("ana going on: %v", err)
	}
	if got, _ := f.store.Skills(ctx, "t1", r.Session); strings.Join(got, " ") != "cards@1" {
		t.Fatalf("the session holds %v", got)
	}
}

// A skill added to the session mid-conversation takes effect from the next
// model turn, appended after what the context held, and never inside a
// tool's answer; adding it again changes nothing.
func TestASkillAddedMidConversationTakesEffectFromTheNextTurn(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_refund", `{"charge":"ch_1"}`), say("Refunded."))
	withSkills(t, f)
	f.refund.result = tool.Result{Text: "refunded re_1"}
	f.refund.onCall = func() {
		// The service that answers the tool adds the skill, as tafs cloud does.
		for range 2 {
			if got, err := f.store.AddSkills(ctx, "t1", f.get(f.runs0().ID).Session, []string{"cards@1"}); err != nil || strings.Join(got, " ") != "wakes@1 cards@1" {
				t.Errorf("added: %v, %v", got, err)
			}
		}
	}
	r := f.start(server.Start{Input: "refund ch_1", Skills: []string{"wakes@1"}})
	if _, err := f.r.Execute(ctx, "t1", r.ID, nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.model.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d turns", len(reqs))
	}
	first, second := reqs[0].System, reqs[1].System
	if strings.Contains(first, "# Skill: cards") || !strings.Contains(first, "# Skill: wakes") {
		t.Fatalf("the first turn's context:\n%s", first)
	}
	if !strings.HasPrefix(second, first) || !strings.HasSuffix(second, "# Skill: cards\nThe cards procedure: step one, step two.") {
		t.Fatalf("the second turn's context isn't the first with the skill appended:\n%s", second)
	}
	// The tool's answer is the tool's, word for word.
	last := reqs[1].Messages[len(reqs[1].Messages)-1].Blocks[0]
	if last.Result != "refunded re_1" {
		t.Fatalf("the tool's answer carried something else: %q", last.Result)
	}
}
