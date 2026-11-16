package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/registry/registrytest"
	"github.com/kiff/agents/runner"
	"github.com/kiff/agents/skill"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
)

// routeModel answers a watch request as told: hands it to a taf by name with
// taf_task (pick says which, by the request; empty hands nothing), then
// answers line. It reads a skill first when read is set.
type routeModel struct {
	pick map[string]string
	line string
	read string
	reqs []model.Request
}

func (m *routeModel) Turn(_ context.Context, req model.Request, _ func(string)) (model.Reply, error) {
	m.reqs = append(m.reqs, req)
	usage := model.Usage{InputTokens: 10, OutputTokens: 5}
	answered := map[string]bool{}
	for _, msg := range req.Messages {
		for _, b := range msg.Blocks {
			if b.Kind == model.ToolResult {
				answered[b.CallID] = true
			}
		}
	}
	switch {
	case answered["t1"]:
		return model.Reply{Blocks: []model.Block{{Kind: model.Text, Text: m.line}}, Stop: model.EndTurn, Usage: usage}, nil
	case m.read != "" && !answered["r1"]:
		return model.Reply{Blocks: []model.Block{{Kind: model.ToolUse, CallID: "r1", Tool: skill.ToolName, Input: json.RawMessage(fmt.Sprintf(`{"name":%q}`, m.read))}},
			Stop: model.CallTools, Usage: usage}, nil
	}
	return m.hand(firstText(req.Messages), usage), nil
}

func (m *routeModel) hand(ask string, usage model.Usage) model.Reply {
	if taf := m.pick[ask]; taf != "" {
		return model.Reply{Blocks: []model.Block{{Kind: model.ToolUse, CallID: "t1", Tool: "taf_task", Input: json.RawMessage(fmt.Sprintf(`{"taf":%q,"text":%q}`, taf, ask))}},
			Stop: model.CallTools, Usage: usage}
	}
	return model.Reply{Blocks: []model.Block{{Kind: model.Text, Text: m.line}}, Stop: model.EndTurn, Usage: usage}
}

// firstText is the person's own message: the first message's last text (the
// runner puts the day before it).
func firstText(msgs []model.Message) string {
	out := ""
	for _, b := range msgs[0].Blocks {
		if b.Kind == model.Text {
			out = b.Text
		}
	}
	return out
}

// routeTest is a route run on m over a definition naming the watch skill
// (named only) and the phone's set-up skill, with the ten tafs.
func routeTest(t *testing.T, m model.Model, withWatchSkill bool) (*test, *bytes.Buffer) {
	t.Helper()
	ctx := context.Background()
	skills := memory.NewSkills()
	d := registrytest.Definition("e2e-route", "setup")
	for _, name := range []string{"set-up-a-taf", routeSkill} {
		if err := skills.Put(ctx, skill.Package{Tenant: "e2e-route", Name: name, Version: "1", Description: "how to " + name, Body: "The " + name + " procedure."}); err != nil {
			t.Fatal(err)
		}
	}
	d.Skills = []skill.Ref{{Name: "set-up-a-taf", Version: "1"}}
	if withWatchSkill {
		d.Skills = append(d.Skills, skill.Ref{Name: routeSkill, Version: "1", NamedOnly: true})
	}
	d.Keep = keepAll
	agents := memory.NewAgents()
	if err := agents.Put(ctx, d); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	tt := &test{tenant: "e2e-route", sessions: memory.New(), route: newRouteTools(), out: &redacting{w: io.Discard, ci: true}, cfg: config{log: &log}}
	tt.rn = &runner.Runner{Runs: memory.NewRuns(), Sessions: tt.sessions, Builder: &registry.Builder{
		Agents: agents, Secrets: fixedKey{}, Model: m, Gateways: []string{"https://gateway.test/mcp"}, Skills: skills,
		Connect: func(context.Context, string, string, string, string) ([]tool.Tool, error) {
			return tt.route.tools(), nil
		},
	}}
	for _, r := range d.Skills {
		tt.setupSkillRefs = append(tt.setupSkillRefs, r.String())
	}
	return tt, &log
}

func rightPicks() map[string]string {
	picks := map[string]string{}
	for _, c := range routeCases {
		for _, x := range routeTafs {
			if x.ID == c.taf {
				picks[c.ask] = x.Name
			}
		}
	}
	return picks
}

// Coach handing each request to its own taf, and nothing to the ambiguous or
// none-can ones, passes every check; the watch session holds the watch skill
// alone, under its limit, and the phone chat never reaches it.
func TestARouteRunThatPicksRightPasses(t *testing.T) {
	m := &routeModel{pick: rightPicks(), line: "Sent."}
	tt, log := routeTest(t, m, true)
	if err := tt.routeChats(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	for _, want := range []string{routeRightTaf, routeOnce, routeNoGuess, routeWatchOnly, routeLimitKept, routeShortLine, routePhoneNoWatch} {
		if !strings.Contains(log.String(), "pass  "+want) {
			t.Errorf("no pass for %q:\n%s", want, log)
		}
	}
	if !strings.Contains(log.String(), "skip  "+routeSaysWhy) {
		t.Errorf("says-why isn't skipped without a judge:\n%s", log)
	}
	// A watch request's model saw only the watch's tools and skill.
	req := m.reqs[0]
	var offered []string
	for _, s := range req.Tools {
		offered = append(offered, s.Name)
	}
	if strings.Join(offered, " ") != "tafs_state taf_task read_skill" || strings.Contains(req.System, "- set-up-a-taf") {
		t.Fatalf("the watch session was offered %v, with:\n%s", offered, req.System)
	}
}

// A wrong taf, a guess at the ambiguous request, and a long answer each fail
// their check, by case name.
func TestARouteRunThatGuessesFails(t *testing.T) {
	picks := rightPicks()
	picks["Move my four o'clock call to six."] = "Inbox"
	picks["Answer the customer who wrote about the late delivery."] = "Tickets"
	m := &routeModel{pick: picks, line: strings.Repeat("Sent. ", 40)}
	tt, log := routeTest(t, m, true)
	err := tt.routeChats(context.Background())
	if err == nil {
		t.Fatalf("passed:\n%s", log)
	}
	for _, want := range []string{"FAIL  " + routeRightTaf + ": handed to no taf or the wrong one: calendar",
		"FAIL  " + routeNoGuess + ": handed over when no single taf could: ambiguous", "FAIL  " + routeShortLine} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("no %q:\n%s", want, log)
		}
	}
}

// A watch session can't read a phone skill: read_skill refuses it there, and
// the check holds.
func TestAWatchSessionThatTriesAPhoneSkillReadsNothing(t *testing.T) {
	m := &routeModel{pick: rightPicks(), line: "Sent.", read: "set-up-a-taf"}
	tt, log := routeTest(t, m, true)
	if err := tt.routeChats(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	if !strings.Contains(log.String(), "pass  "+routeWatchOnly) {
		t.Fatalf("a refused read counted:\n%s", log)
	}
}

// A definition from before the watch's way in names no watch skill: the run
// says so and runs nothing.
func TestARouteRunNeedsTheWatchSkill(t *testing.T) {
	tt, _ := routeTest(t, &routeModel{}, false)
	if err := tt.routeChats(context.Background()); err == nil || !strings.Contains(err.Error(), "names no ask-a-taf") {
		t.Fatalf("got %v", err)
	}
}

func TestTheRouteFlag(t *testing.T) {
	if err := routeFlag(config{}); err != nil {
		t.Fatal(err)
	}
	if err := routeFlag(config{desk: true}); err == nil {
		t.Fatal("-route with -desk")
	}
	if err := routeFlag(config{plan: &plan{}}); err == nil {
		t.Fatal("-route with -plan")
	}
	if len(routeTafs) != 10 {
		t.Fatalf("%d tafs", len(routeTafs))
	}
}
