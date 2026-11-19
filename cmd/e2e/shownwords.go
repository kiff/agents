package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/session"
)

// shownNotes are Setup's progress notes as the agent emitted them, in order.
// A note the wording rules dropped never reached the owner, so it is never
// graded as one of Setup's lines (the model's own stream still has it).
type shownNotes struct {
	mu    sync.Mutex
	notes []string
}

// emit is the runs' emit: it keeps each Update event's note.
func (s *shownNotes) emit(e agent.Event) {
	if e.Kind != agent.Update || strings.TrimSpace(e.Text) == "" {
		return
	}
	s.mu.Lock()
	s.notes = append(s.notes, e.Text)
	s.mu.Unlock()
}

func (s *shownNotes) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.notes)
}

// wordingLine is the public log's line for what the wording rules did to
// Setup's own words: messages held and sent back to be reworded, messages
// said as written all the same (past the limit, or in a closing turn: a
// miss), and progress notes dropped. Counts only, never a word.
func wordingLine(events []session.Event) string {
	held, said, dropped := 0, 0, 0
	for _, e := range events {
		if e.Kind != session.OwnRefused {
			continue
		}
		var b agent.OwnRefusedBody
		if json.Unmarshal(e.Body, &b) != nil {
			continue
		}
		switch {
		case b.Tool == agent.MessageTool && b.Said:
			said++
		case b.Tool == agent.MessageTool:
			held++
		case b.Tool == agent.UpdateNote:
			dropped++
		}
	}
	return fmt.Sprintf("setup wording: %d messages held and reworded, %d said as written (misses), %d notes dropped", held, said, dropped)
}

// unshownConfirms counts the confirms left unmade because the rules notice
// they agree to was never shown (agent.UnshownNotice): the notice was
// refused, so the owner saw neither, and the step fails for them.
func unshownConfirms(events []session.Event) int {
	n := 0
	for _, e := range events {
		if e.Kind != session.OwnRefused {
			continue
		}
		var b agent.OwnRefusedBody
		if json.Unmarshal(e.Body, &b) == nil && b.Error == agent.UnshownNotice {
			n++
		}
	}
	return n
}
