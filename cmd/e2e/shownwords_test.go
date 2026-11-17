package main

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/session"
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
