package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/chat"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/registry/registrytest"
	runs "github.com/kiff/agents/run"
	"github.com/kiff/agents/runner"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/skill"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
)

// A session that called read_skill for change-a-taf, then another tool, then
// read_skill again for explain-a-hold.
func readingSession(t *testing.T) *test {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	sessions := memory.New()
	if err := sessions.Create(ctx, session.Session{ID: "s1", Tenant: "t1", Agent: "setup", Started: now, Updated: now}); err != nil {
		t.Fatal(err)
	}
	if err := sessions.Begin(ctx, "t1", "s1", "r1", now); err != nil {
		t.Fatal(err)
	}
	call := func(seq int64, tool, input string) session.Event {
		b, _ := json.Marshal(agent.ToolCallBody{CallID: "c", Tool: tool, Input: json.RawMessage(input)})
		return session.Event{Session: "s1", Run: "r1", Seq: seq, Time: now, Kind: session.ToolCall, Body: b}
	}
	events := []session.Event{
		call(1, skill.ToolName, `{"name":"change-a-taf"}`),
		call(2, "kiff_card", `{"agent":"rae"}`),
		call(3, skill.ToolName, `{"name":"explain-a-hold","file":"refs.md"}`),
	}
	if err := sessions.Append(ctx, "t1", "s1", "r1", events); err != nil {
		t.Fatal(err)
	}
	return &test{tenant: "t1", sessions: sessions, cfg: config{log: io.Discard}}
}

func TestSkillsReadAreTheReadSkillCallsInOrder(t *testing.T) {
	tt := readingSession(t)
	read, err := tt.skillsRead(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(read, ",") != "change-a-taf,explain-a-hold" {
		t.Fatalf("read %v", read)
	}
}

func TestReadItAndReadNone(t *testing.T) {
	tt := readingSession(t)
	ctx := context.Background()
	if err := tt.readIt(ctx, "s1", "explain-a-hold", "asked"); err != nil {
		t.Fatalf("a skill it read: %v", err)
	}
	if err := tt.readIt(ctx, "s1", "halt-and-resume", "asked"); err == nil || !strings.Contains(err.Error(), "didn't read halt-and-resume") {
		t.Fatalf("a skill it didn't read: %v", err)
	}
	if err := tt.readNone(ctx, "s1"); err == nil || !strings.Contains(err.Error(), "change-a-taf, explain-a-hold") {
		t.Fatalf("setting up read skills: %v", err)
	}
}

// scriptedReads is a model that reads a skill on its first turn, then answers: what the
// e2e's skill cases look for, end to end through the runner, its recording and the store.
type scriptedReads struct {
	turns int
	reqs  []model.Request
}

func (s *scriptedReads) Turn(_ context.Context, req model.Request, _ func(string)) (model.Reply, error) {
	s.turns++
	s.reqs = append(s.reqs, req)
	if s.turns == 1 {
		return model.Reply{Blocks: []model.Block{{Kind: model.ToolUse, CallID: "c1", Tool: skill.ToolName, Input: json.RawMessage(`{"name":"change-a-taf"}`)}},
			Stop: model.CallTools, Usage: model.Usage{InputTokens: 10, OutputTokens: 5}}, nil
	}
	return model.Reply{Blocks: []model.Block{{Kind: model.Text, Text: "done"}}, Stop: model.EndTurn, Usage: model.Usage{InputTokens: 10, OutputTokens: 5}}, nil
}

type fixedKey struct{}

func (fixedKey) Key(context.Context, string, string) (string, error) { return "key", nil }

// The skill cases see a read made through the runner, as in the e2e: the definition names
// the skill, its deferred tools are Setup's, and the session keeps tool calls (keepAll).
func TestSkillsReadThroughTheRunner(t *testing.T) {
	ctx := context.Background()
	skills := memory.NewSkills()
	md := "---\nname: change-a-taf\ndescription: Read it first when the conversation starts with [improve taf <id>].\n---\n\n# Change a taf\n\nAs the next version.\n"
	name, desc, body, err := skill.Parse(md)
	if err != nil {
		t.Fatal(err)
	}
	if err := skills.Put(ctx, skill.Package{Tenant: "t1", Name: name, Version: "1", Description: desc, Body: body}); err != nil {
		t.Fatal(err)
	}
	d := registrytest.Definition("t1", "setup")
	d.Skills = []skill.Ref{{Name: "change-a-taf", Version: "1"}}
	d.DeferredTools = []string{"tafs_state", "taf_run", "taf_task", "run_stop", "taf_pause", "version_try", "version_install"}
	d.DeferVendorTools = true
	d.Keep = keepAll
	agents := memory.NewAgents()
	if err := agents.Put(ctx, d); err != nil {
		t.Fatal(err)
	}
	m := &scriptedReads{}
	sessions := memory.New()
	rn := &runner.Runner{Runs: memory.NewRuns(), Sessions: sessions, Builder: &registry.Builder{
		Agents: agents, Secrets: fixedKey{}, Model: m, Gateways: []string{"https://gateway.test/mcp"}, Skills: skills,
		Connect: func(context.Context, string, string) ([]tool.Tool, error) { return nil, nil },
	}}
	ch := &chat.Chat{Runner: rn, Tenant: "t1", Agent: "setup"}
	r, err := ch.Send(ctx, "[improve taf taf-1]\nLet it refund up to 10 euros.", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != runs.Completed {
		t.Fatalf("the run ended %s", r.Status)
	}
	// What the model was offered: the skill list in the system prompt, read_skill not deferred.
	req := m.reqs[0]
	if !strings.Contains(req.System, "# Skills") || !strings.Contains(req.System, "change-a-taf") {
		t.Fatal("the system prompt has no skill list")
	}
	var offered bool
	for _, s := range req.Tools {
		if s.Name == skill.ToolName {
			offered = !s.Defer
		}
	}
	if !offered {
		t.Fatal("read_skill isn't offered, or is deferred")
	}
	tt := &test{tenant: "t1", sessions: sessions, cfg: config{log: io.Discard}}
	read, err := tt.skillsRead(ctx, ch.Session)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(read, ",") != "change-a-taf" {
		t.Fatalf("read %v: the skill cases can't see a read the runner made", read)
	}
}
