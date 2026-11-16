package runner_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/registry/registrytest"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/skill"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
)

// withOneWayIn gives the fixture's agent three skills: cards and wakes for
// any session, and ask, named only by a session that opens with it.
func withOneWayIn(t *testing.T, f *fixture) {
	t.Helper()
	skills := memory.NewSkills()
	for _, name := range []string{"cards", "wakes", "ask"} {
		if err := skills.Put(ctx, skill.Package{Tenant: "t1", Name: name, Version: "1", Description: "how to " + name, Body: "The " + name + " procedure."}); err != nil {
			t.Fatal(err)
		}
	}
	f.r.Builder.Skills = skills
	d := registrytest.Definition("t1", "rae")
	d.Skills = []skill.Ref{{Name: "cards", Version: "1"}, {Name: "wakes", Version: "1"}, {Name: "ask", Version: "1", NamedOnly: true}}
	if err := f.agents.Put(ctx, d); err != nil {
		t.Fatal(err)
	}
	f.extra = []tool.Tool{&gatewayTool{name: "tafs_state", result: tool.Result{Text: "{}"}}, &gatewayTool{name: "taf_task", result: tool.Result{Text: "handed"}}}
}

func toolNames(specs []model.ToolSpec) []string {
	var out []string
	for _, s := range specs {
		out = append(out, s.Name)
	}
	slices.Sort(out)
	return out
}

func skillEnum(t *testing.T, specs []model.ToolSpec) string {
	t.Helper()
	for _, s := range specs {
		if s.Name == skill.ToolName {
			return string(s.InputSchema)
		}
	}
	return ""
}

// A session opened with a limit is offered only the tools it names, and with
// skills, reads only its own: read_skill lists and reads them alone, and the
// agent's catalog of the rest is gone from its instructions. Its next run is
// limited the same, the limit kept with the session.
func TestALimitedSessionIsOfferedOnlyItsToolsAndSkills(t *testing.T) {
	f := newFixture(t, use("c1", skill.ToolName, `{"name":"cards"}`), say("Sent."), say("Again."))
	withOneWayIn(t, f)
	only := &session.Only{Skills: true, Tools: []string{"tafs_state", "taf_task"}}
	r := f.start(server.Start{Input: "move my call", Requester: ana, Skills: []string{"ask@1"}, Only: only})
	if _, err := f.r.Execute(ctx, "t1", r.ID, nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.model.requests()
	if got := strings.Join(toolNames(reqs[0].Tools), " "); got != "read_skill taf_task tafs_state" {
		t.Fatalf("offered %s", got)
	}
	if enum := skillEnum(t, reqs[0].Tools); !strings.Contains(enum, `["ask"]`) {
		t.Fatalf("read_skill lists %s", enum)
	}
	if sys := reqs[0].System; strings.Contains(sys, "- cards:") || strings.Contains(sys, "- wakes:") || !strings.Contains(sys, "# Skill: ask\nThe ask procedure.") {
		t.Fatalf("the context:\n%s", sys)
	}
	// Reading another skill by name is refused, as a name it doesn't have.
	res := reqs[1].Messages[len(reqs[1].Messages)-1].Blocks[0]
	if !res.IsError || strings.Contains(res.Result, "cards procedure") {
		t.Fatalf("read_skill of cards answered %+v", res)
	}
	sess, _, err := f.store.Read(ctx, "t1", r.Session)
	if err != nil || sess.Only == nil || !sess.Only.Skills || strings.Join(sess.Only.Tools, " ") != "tafs_state taf_task" {
		t.Fatalf("the session keeps %+v, %v", sess.Only, err)
	}
	next := f.start(server.Start{Session: r.Session, Input: "and tomorrow", Requester: ana})
	if _, err := f.r.Execute(ctx, "t1", next.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(toolNames(f.model.requests()[2].Tools), " "); got != "read_skill taf_task tafs_state" {
		t.Fatalf("the next run is offered %s", got)
	}
}

// A limit is never widened: a later run adds no skill to a session limited
// to its own, nor does the agent's service; nothing is added.
func TestALimitedSessionTakesNoMoreSkills(t *testing.T) {
	f := newFixture(t, say("Sent."))
	withOneWayIn(t, f)
	r := f.start(server.Start{Input: "hi", Requester: ana, Skills: []string{"ask@1"}, Only: &session.Only{Skills: true, Tools: []string{"taf_task"}}})
	_, err := f.r.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Session: r.Session, Input: "x", Requester: ana, Skills: []string{"cards@1"}})
	if !errors.Is(err, registry.ErrSkillsLimited) {
		t.Fatalf("adding cards to the limited session: %v", err)
	}
	// The skill it holds, named again, changes nothing and is no widening.
	if _, err := f.r.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Session: r.Session, Input: "y", Requester: ana, Skills: []string{"ask@1"}}); err != nil {
		t.Fatalf("naming its own skill again: %v", err)
	}
	if got, _ := f.store.Skills(ctx, "t1", r.Session); strings.Join(got, " ") != "ask@1" {
		t.Fatalf("the session holds %v", got)
	}
	// A limit is checked when the session opens: none with no tools.
	if _, err := f.r.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "z", Requester: ana, Only: &session.Only{Skills: true}}); err == nil {
		t.Fatal("a limit naming no tool opened a session")
	}
}

// A named-only skill is the sessions' that name it when they open: any other
// session's catalog and read_skill never show it, it can't be read there by
// name, and no later run adds it.
func TestANamedOnlySkillStaysOutOfEveryOtherSession(t *testing.T) {
	f := newFixture(t, use("c1", skill.ToolName, `{"name":"ask"}`), say("Hello."))
	withOneWayIn(t, f)
	r := f.start(server.Start{Input: "hi", Requester: ana, Skills: []string{"cards@1"}})
	if _, err := f.r.Execute(ctx, "t1", r.ID, nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.model.requests()
	if enum := skillEnum(t, reqs[0].Tools); strings.Contains(enum, "ask") || !strings.Contains(enum, "cards") {
		t.Fatalf("read_skill lists %s", enum)
	}
	if sys := reqs[0].System; strings.Contains(sys, "- ask:") || strings.Contains(sys, "ask procedure") {
		t.Fatalf("the context shows ask:\n%s", sys)
	}
	if res := reqs[1].Messages[len(reqs[1].Messages)-1].Blocks[0]; !res.IsError || strings.Contains(res.Result, "ask procedure") {
		t.Fatalf("read_skill of ask answered %+v", res)
	}
	_, err := f.r.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Session: r.Session, Input: "x", Requester: ana, Skills: []string{"ask@1"}})
	if !errors.Is(err, registry.ErrSkillsLimited) {
		t.Fatalf("adding ask to an open session: %v", err)
	}
	if got, _ := f.store.Skills(ctx, "t1", r.Session); strings.Join(got, " ") != "cards@1" {
		t.Fatalf("the session holds %v", got)
	}
}

// A session without a limit is offered every tool and every skill but the
// named-only ones, as before.
func TestASessionWithoutALimitIsAsBefore(t *testing.T) {
	f := newFixture(t, say("Hello."))
	withOneWayIn(t, f)
	r := f.start(server.Start{Input: "hi", Requester: ana})
	if _, err := f.r.Execute(ctx, "t1", r.ID, nil); err != nil {
		t.Fatal(err)
	}
	req := f.model.requests()[0]
	if got := strings.Join(toolNames(req.Tools), " "); got != "read_skill stripe_refund taf_task tafs_state" {
		t.Fatalf("offered %s", got)
	}
	if !strings.Contains(req.System, "- cards:") || !strings.Contains(req.System, "- wakes:") {
		t.Fatalf("the catalog:\n%s", req.System)
	}
	if sess, _, _ := f.store.Read(ctx, "t1", r.Session); sess.Only != nil {
		t.Fatalf("the session has a limit: %+v", sess.Only)
	}
}
