package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/skill"
	"github.com/kiff/agents/store/memory"
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
