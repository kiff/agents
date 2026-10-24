package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/chat"
	runs "github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/skill"
)

// The skills cases (kiff/tafs#218): Coach keeps in its instructions only what
// every conversation needs, and reads the rest as skills when a conversation
// is about them. What skills can break is a skill not read when it should be,
// or read when it shouldn't. So, when Setup's definition names skills:
//
//   - setting up a new taf reads none of them (the setup chat);
//   - the improve case reads change-a-taf;
//   - each question below reads its skill, though the owner never names it.

// skillAsks are owner questions that need a skill without naming its topic,
// by language, with the skill each must read.
var skillAsks = map[string][]struct{ ask, skill string }{
	"en": {
		{"Refunds hasn't done anything since this morning. What's going on?", "explain-a-hold"},
		{"How much can my tafs still do this month before I have to pay more?", "tasks-and-packs"},
		{"Something's wrong with the refunds. Stop everything right now.", "halt-and-resume"},
	},
	"es": {
		{"Reembolsos no ha hecho nada desde esta mañana. ¿Qué pasa?", "explain-a-hold"},
		{"¿Cuánto pueden hacer todavía mis tafs este mes antes de que tenga que pagar más?", "tasks-and-packs"},
		{"Algo va mal con los reembolsos. Para todo ahora mismo.", "halt-and-resume"},
	},
}

// skillsRead are the skills the agent read in a session, in order, by name.
func (t *test) skillsRead(ctx context.Context, sessionID string) ([]string, error) {
	_, events, err := t.sessions.Read(ctx, t.tenant, sessionID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range events {
		if e.Kind != session.ToolCall {
			continue
		}
		var b agent.ToolCallBody
		if json.Unmarshal(e.Body, &b) != nil || b.Tool != skill.ToolName {
			continue
		}
		var in struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(b.Input, &in) == nil && in.Name != "" {
			out = append(out, in.Name)
		}
	}
	return out, nil
}

// readNone fails when setting up a new taf read a skill: it needs none of them.
func (t *test) readNone(ctx context.Context, sessionID string) error {
	read, err := t.skillsRead(ctx, sessionID)
	if err != nil {
		return err
	}
	fmt.Fprintf(t.cfg.log, "skills read setting up: %d\n", len(read))
	if len(read) > 0 {
		return fmt.Errorf("setting up a new taf read skills it doesn't need: %s", strings.Join(read, ", "))
	}
	return nil
}

// readIt fails when a session didn't read the skill it's about.
func (t *test) readIt(ctx context.Context, sessionID, want, what string) error {
	read, err := t.skillsRead(ctx, sessionID)
	if err != nil {
		return err
	}
	fmt.Fprintf(t.cfg.log, "skills read %s: %s\n", what, strings.Join(read, ", "))
	if !slices.Contains(read, want) {
		return fmt.Errorf("%s didn't read %s", what, want)
	}
	return nil
}

// skillChoice asks each question in a new conversation with Setup, and checks
// it read the skill the question needs. Every question runs before the test
// fails, so one run shows each skill's read.
func (t *test) skillChoice(ctx context.Context) error {
	var failed []string
	for _, c := range skillAsks[t.cfg.owner] {
		if !slices.Contains(t.setupSkills, c.skill) {
			continue
		}
		ch := &chat.Chat{Runner: t.rn, Tenant: t.tenant, Agent: "setup"}
		t.logf("owner: %s", c.ask)
		r, err := ch.Send(ctx, c.ask, nil)
		if err != nil {
			return fmt.Errorf("the setup agent's run: %w", err)
		}
		if r.Status != runs.Completed && r.Status != runs.Paused {
			return fmt.Errorf("the setup agent's run ended %s", r.Status)
		}
		if _, text, err := t.latest(ctx, ch.Session, r.ID); err == nil && text != "" {
			t.logf("agent: %s", oneLine(text))
		}
		if err := t.readIt(ctx, ch.Session, c.skill, "asked about "+c.skill); err != nil {
			failed = append(failed, err.Error())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("skills not read: %s", strings.Join(failed, "; "))
	}
	return nil
}
