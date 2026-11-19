package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool/show"
)

// Setup's notes are graded as the owner saw them: the agent's Update events,
// never a note the wording rules dropped before it was emitted, nor anything
// else the run emitted.
func TestOnlyTheNotesTheAgentEmittedAreKept(t *testing.T) {
	var s shownNotes
	s.emit(agent.Event{Kind: agent.Update, Text: "Miro la cuenta…"})
	s.emit(agent.Event{Kind: agent.Thought, Text: "thinking"})
	s.emit(agent.Event{Kind: agent.Update, Text: "  "})
	s.emit(agent.Event{Kind: agent.Update, Text: "Connecting your sheet…"})
	if got := s.all(); !slices.Equal(got, []string{"Miro la cuenta…", "Connecting your sheet…"}) {
		t.Fatalf("notes %q", got)
	}
}

func TestWordingLineCountsWhatTheRulesDid(t *testing.T) {
	refused := func(b agent.OwnRefusedBody) session.Event {
		raw, _ := json.Marshal(b)
		return session.Event{Kind: session.OwnRefused, Body: raw}
	}
	events := []session.Event{
		refused(agent.OwnRefusedBody{Tool: agent.MessageTool, Error: "your message says …"}),
		refused(agent.OwnRefusedBody{Tool: agent.MessageTool, Error: "your message says …"}),
		refused(agent.OwnRefusedBody{Tool: agent.MessageTool, Error: "your message says …", Said: true}),
		refused(agent.OwnRefusedBody{Tool: agent.UpdateNote, Error: "your message says …"}),
		refused(agent.OwnRefusedBody{Tool: "show", Error: "a card refused"}), // a card: not the message rules' to count
		{Kind: session.AgentMessage, Body: json.RawMessage(`{"text":"hi"}`)},
	}
	want := "setup wording: 2 messages held and reworded, 1 said as written (misses), 1 notes dropped"
	if got := wordingLine(events); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := wordingLine(nil); got != "setup wording: 0 messages held and reworded, 0 said as written (misses), 0 notes dropped" {
		t.Fatalf("none: %q", got)
	}
}

// A confirm left unmade because its rules notice was refused is counted; a
// card refused and redrawn, or a wording miss, isn't.
func TestUnshownConfirmsAreCounted(t *testing.T) {
	refused := func(b agent.OwnRefusedBody) session.Event {
		raw, _ := json.Marshal(b)
		return session.Event{Kind: session.OwnRefused, Body: raw}
	}
	events := []session.Event{
		refused(agent.OwnRefusedBody{Tool: "show", Error: "no lines and no lang"}),
		refused(agent.OwnRefusedBody{Tool: "show", Error: agent.UnshownNotice}),
		refused(agent.OwnRefusedBody{Tool: agent.MessageTool, Error: "your message says …"}),
		{Kind: session.AgentMessage, Body: json.RawMessage(`{"text":"Agree?"}`)},
	}
	if got := unshownConfirms(events); got != 1 {
		t.Fatalf("unshown confirms: %d, want 1", got)
	}
	if got := unshownConfirms(events[:1]); got != 0 {
		t.Fatalf("a refused card alone: %d, want 0", got)
	}
}

// A card Setup sent with no text, which show refused, is counted as its own
// line, whether or not Coach recovered; another refusal of a card isn't. The
// refusals are show's own words, read from show.New().Call, never typed here:
// Parse names a card by its type and id ("notice rules: text is required").
func TestEmptyCardsAreCounted(t *testing.T) {
	shown := func(card string) string {
		res, err := show.New().Call(context.Background(), json.RawMessage(card))
		if err != nil || !res.IsError {
			t.Fatalf("show should refuse %s: %+v %v", card, res, err)
		}
		return res.Text
	}
	refused := func(tool, why string) session.Event {
		raw, _ := json.Marshal(agent.OwnRefusedBody{Tool: tool, Error: why})
		return session.Event{Kind: session.OwnRefused, Body: raw}
	}
	empty := shown(`{"type":"notice","id":"rules","tone":"info","title":"Your rules","text":""}`)
	if !strings.HasSuffix(empty, ": text is required") {
		t.Fatalf("show says %q: the count's match no longer fits it", empty)
	}
	events := []session.Event{
		refused("show", empty),
		refused("show", agent.UnshownNotice),
		refused("show", "no lines and no lang"),
		refused(agent.MessageTool, empty), // not a card
		refused("show", shown(`{"type":"notice","id":"n1","tone":"info","title":"x"}`)),
		{Kind: session.AgentMessage, Body: json.RawMessage(`{"text":"Agree?"}`)},
	}
	if got := emptyCards(events); got != 2 {
		t.Fatalf("empty cards: %d, want 2", got)
	}
	if got, want := emptyCardsLine(events), "setup cards sent with no text and refused: 2"; got != want {
		t.Fatalf("line %q, want %q", got, want)
	}
	if got := emptyCards(nil); got != 0 {
		t.Fatalf("none: %d", got)
	}
	if emptyCardsFailure(0) != nil || emptyCardsFailure(1) == nil {
		t.Fatal("a setup fails on one empty card, and not on none")
	}
}
