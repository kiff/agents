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
//   - setting up a new taf reads none of them (the setup chat): since
//     kiff/tafs#525 the start loads the setup's skills, and none is read;
//   - the improve case has improve-a-taf (loaded by the start; change-a-taf,
//     read, under kiff/tafs#218);
//   - each question below has its skill, though the owner never names it:
//     loaded by the account's state in tafs cloud (an account with a taf
//     starts with the holds and the halting), or read by the agent.

// skillAsks are owner questions that need a skill without naming its topic,
// by language, with the skill each must read.
var skillAsks = map[string][]struct {
	ask   string
	skill []string
}{
	"en": {
		{"Refunds hasn't done anything since this morning. What's going on?", []string{"cards-and-holds", "explain-a-hold"}},
		{"How much can my tafs still do this month before I have to pay more?", []string{"usage-and-plans", "tasks-and-packs"}},
		{"Something's wrong with the refunds. Stop everything right now.", []string{"halting", "halt-and-resume"}},
	},
	"es": {
		{"Reembolsos no ha hecho nada desde esta mañana. ¿Qué pasa?", []string{"cards-and-holds", "explain-a-hold"}},
		{"¿Cuánto pueden hacer todavía mis tafs este mes antes de que tenga que pagar más?", []string{"usage-and-plans", "tasks-and-packs"}},
		{"Algo va mal con los reembolsos. Para todo ahora mismo.", []string{"halting", "halt-and-resume"}},
	},
}

// skillOf is the name the definition has of a question's skill (the new name, or
// kiff/tafs#218's), "" when it names neither.
func (t *test) skillOf(names []string) string {
	for _, n := range names {
		if slices.Contains(t.setupSkills, n) {
			return n
		}
	}
	return ""
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
		skill := t.skillOf(c.skill)
		if skill == "" {
			continue
		}
		// A free-text start on an account that has its taf by now, with no held call or halt: the
		// setup's skills and what tafs cloud adds for an account with a taf (the holds, the
		// halting). A skill neither loaded so nor read by the agent fails the case.
		ch := &chat.Chat{Runner: t.rn, Tenant: t.tenant, Agent: "setup", Skills: t.startSkills("free", true)}
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
		if err := t.hasIt(ctx, ch.Session, skill, "asked about "+skill); err != nil {
			failed = append(failed, err.Error())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("skills not read: %s", strings.Join(failed, "; "))
	}
	return nil
}
