package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/session"
)

// Which of Coach's skills a conversation opens with, as tafs cloud decides it
// (kiff/tafs#525, runtime/src/cloud/coach-skills.ts, way 1): the e2e drives
// Setup without tafs cloud, so it names them the same way, from the entry
// point. Only skills the definition names are sent; an older definition
// (kiff/tafs#218's names, or none) sends none, and the case runs as before.
var coachStart = map[string][]string{
	"free":    {"set-up-a-taf", "connect-an-app", "name-the-tools", "draft-the-card", "usage-and-plans"},
	"improve": {"improve-a-taf", "connect-an-app", "name-the-tools", "draft-the-card"},
}

// coachWithTaf is what an account with a taf of its own adds to every start,
// whatever its state: an owner asks why a taf did nothing, or to stop
// everything, in words that name no skill (tafs cloud's CoachState.hasTaf).
var coachWithTaf = []string{"cards-and-holds", "halting"}

// startSkills is the entry point's skills as stored versions, in the
// definition's order, as kiff/agents keeps them. hasTaf says the account has
// a taf of its own by then: the first setup chat doesn't, every chat after it
// does.
func (t *test) startSkills(entry string, hasTaf bool) []string {
	want := coachStart[entry]
	if hasTaf && len(want) > 0 {
		want = append(slices.Clone(want), coachWithTaf...)
	}
	var out []string
	for _, ref := range t.setupSkillRefs {
		name, _, _ := strings.Cut(ref, "@")
		if slices.Contains(want, name) {
			out = append(out, ref)
		}
	}
	return out
}

// loadedOrRead reports whether a session has skill (by name): loaded by
// tafs cloud's rule on the session, or read by the agent (read_skill), and
// which. An error reads nothing.
func (t *test) loadedOrRead(ctx context.Context, sessionID, name string) (how string, err error) {
	s, _, err := t.sessions.Read(ctx, t.tenant, sessionID)
	if err != nil {
		return "", err
	}
	for _, ref := range s.Skills {
		if n, _, _ := strings.Cut(ref, "@"); n == name {
			return "loaded", nil
		}
	}
	read, err := t.skillsRead(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if slices.Contains(read, name) {
		return "read", nil
	}
	return "", nil
}

// hasIt fails when a session has neither loaded nor read the skill it's about.
func (t *test) hasIt(ctx context.Context, sessionID, want, what string) error {
	how, err := t.loadedOrRead(ctx, sessionID, want)
	if err != nil {
		return err
	}
	fmt.Fprintf(t.cfg.log, "skill %s %s: %s\n", want, what, map[string]string{"": "neither loaded nor read", "loaded": "loaded by the start", "read": "read by the agent"}[how])
	if how == "" {
		return fmt.Errorf("%s had no %s: neither loaded by the start nor read", what, want)
	}
	return nil
}

// What Setup may say of a connection's mode depends on what the connection
// reports (kiff/tafs#525's core rule): a judge decides it, told what the
// connection reported (setupgrade.go, modeRule). planque's fake vendor reports
// test (its accounts read, a charge's livemode false); the desk, no mode.

// reportsTest is a tool result that says the account is in test mode: a
// vendor object with livemode false, or an accounts read with mode test.
var reportsTest = regexp.MustCompile(`"livemode"\s*:\s*false|"mode"\s*:\s*"test"`)

// beforeTestReport walks a run's events in order: what Setup stated (its
// messages and its cards' statements, never the options a card offers) while no tool result in the
// conversation had reported test mode, and whether one has by the run's end.
// reported is the conversation's state before the run.
func beforeTestReport(events []session.Event, runID string, reported bool) ([]string, bool) {
	var before []string
	for _, e := range events {
		if e.Run != runID {
			continue
		}
		switch e.Kind {
		case session.ToolResult:
			var b agent.ToolResultBody
			if json.Unmarshal(e.Body, &b) == nil && !b.IsError && reportsTest.MatchString(b.Text) {
				reported = true
			}
		case session.AgentMessage, session.Report:
			var b agent.TextBody
			if !reported && json.Unmarshal(e.Body, &b) == nil && strings.TrimSpace(b.Text) != "" {
				before = append(before, b.Text)
			}
		case session.Component:
			var c component
			if !reported && json.Unmarshal(e.Body, &c) == nil {
				before = append(before, cardStatements(c)...)
			}
		}
	}
	return before, reported
}

// noteTestReport adds a run's texts said before test mode was reported to
// what the grader reads (said.beforeTest).
func (t *test) noteTestReport(ctx context.Context, sessionID, runID string) error {
	_, events, err := t.sessions.Read(ctx, t.tenant, sessionID)
	if err != nil {
		return err
	}
	before, reported := beforeTestReport(events, runID, t.said.testReported)
	t.said.beforeTest = append(t.said.beforeTest, before...)
	t.said.testReported = reported
	return nil
}

// markFacts notes where the setup session stands when the owner gives their
// limits: the number of its events so far (said.factsAt). A card the owner
// answers belongs to the latest run, so what Setup writes next comes after.
func (t *test) markFacts(ctx context.Context, sessionID string) error {
	_, events, err := t.sessions.Read(ctx, t.tenant, sessionID)
	if err != nil {
		return err
	}
	t.said.factsAt, t.said.factsMarked = len(events), true
	return nil
}

// textsAfter is what Setup wrote (messages and cards) in events from index from on.
func textsAfter(events []session.Event, from int) []string {
	var out []string
	for i, e := range events {
		if i < from {
			continue
		}
		switch e.Kind {
		case session.AgentMessage, session.Report:
			var b agent.TextBody
			if json.Unmarshal(e.Body, &b) == nil && strings.TrimSpace(b.Text) != "" {
				out = append(out, b.Text)
			}
		case session.Component:
			var c component
			if json.Unmarshal(e.Body, &c) == nil {
				out = append(out, cardTexts(c)...)
			}
		}
	}
	return out
}
