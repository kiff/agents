package agent

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool"
)

// CalledBody is the body of a Called event: one call a run made to a tool
// outside the agent, and what came of it, in words. It outlives the call's
// own events, which a run drops when it ends, so a chat's next runs still
// know what was done: a look-up made in one message counts in the next,
// and a refund named in the chat has its call behind it (kiff/tafs, the
// R-3005 test: after an approved hold the taf said it hadn't looked the
// payment up, and first named the amount of an earlier refund).
type CalledBody struct {
	Tool    string          `json:"tool"`
	Input   json.RawMessage `json:"input,omitempty"`
	Outcome string          `json:"outcome"`
}

// CalledInputMax is the most of a call's input a Called event keeps, in
// bytes: enough for the ids and amounts a call acts on.
const CalledInputMax = 512

// The outcomes a Called event names.
const (
	OutcomeDone     = "done"
	OutcomeFailed   = "failed"
	OutcomeNotMade  = "not made: it was sent beside a read"
	OutcomeHeld     = "held by KIFF for the owner"
	OutcomeRefused  = "refused by KIFF, not sent"
	OutcomeApproved = "held by KIFF, approved by the owner, then sent once: done"
)

// RecordNote follows a continued run's held calls' results: what the run did,
// from its record. The model says what was done from it, never from its
// memory of the chat.
const RecordNote = "This run's record, call by call, in order. Say what was done from this record, with its amounts and ids, not from earlier messages in the chat. Never say a step was skipped that this record shows was made."

// outcome is what came of a call, from its result. answer is the owner's
// answer to its hold, when it was held and has been answered.
func outcome(r ToolResultBody, answer string) string {
	switch {
	case r.NotMade:
		return OutcomeNotMade
	case r.Held():
		return OutcomeHeld
	case answer != "" && answer != "approved":
		return "held by KIFF, " + answer + ", not sent"
	case r.State == tool.StateRefused:
		return OutcomeRefused
	case r.State == "unknown":
		return "sent, but whether it took effect is unknown"
	case r.IsError:
		return OutcomeFailed
	case answer == "approved":
		return OutcomeApproved
	}
	return OutcomeDone
}

// recordCalled records a call to a tool outside the agent and its outcome.
func recordCalled(rec *session.Recording, t tool.Tool, name string, input json.RawMessage, oc string) error {
	if t != nil && t.Access() == tool.Own {
		return nil
	}
	return rec.Record(session.Called, CalledBody{Tool: name, Input: cutInput(input), Outcome: oc})
}

// cutInput is input within CalledInputMax, as a JSON string when it had to
// be cut, so what is kept stays valid JSON.
func cutInput(input json.RawMessage) json.RawMessage {
	if len(input) <= CalledInputMax {
		return input
	}
	cut, _ := json.Marshal(strings.ToValidUTF8(string(input[:CalledInputMax]), "") + "…")
	return cut
}

// CalledMark opens each line of a run's record, and only those: the model
// trusts what follows it as what was done.
const CalledMark = "[called]"

// forgedMark is the mark in any spelling the model would read as it.
var forgedMark = regexp.MustCompile(`(?i)\[\s*called\s*\]`)

// Unforged is text the runner didn't write as a record (an owner's message,
// a tool's result, a customer's email, the agent's own words) with the
// record's mark taken out, so nothing that only looks like a line of the
// record is read as one.
func Unforged(text string) string {
	return forgedMark.ReplaceAllString(text, "(called)")
}

// CalledLine is a Called event as the model reads it: the only text that
// carries CalledMark.
func CalledLine(b CalledBody) string {
	line := CalledMark + " " + Unforged(b.Tool)
	if len(b.Input) > 0 {
		line += " " + Unforged(string(b.Input))
	}
	return line + " → " + b.Outcome
}

// recordNote is what a continued run did, from its record: each call of its
// turns before the pause and of the paused turn, with its outcome; the held
// ones with the owner's answer. Empty when it called nothing outside the
// agent.
func (a Agent) recordNote(r run.Run, held []ToolResultBody) string {
	own := map[string]bool{}
	for _, t := range a.Tools {
		if t.Access() == tool.Own {
			own[t.Spec().Name] = true
		}
	}
	answers := map[string]string{}
	for _, q := range r.Requirements {
		answers[q.CallID] = q.Answer
	}
	byCall := map[string]ToolResultBody{}
	for _, h := range held {
		byCall[h.CallID] = h
	}
	for _, m := range append(copied(r.Paused.Before), model.Message{Role: model.User, Blocks: r.Paused.Results}) {
		for _, b := range m.Blocks {
			if b.Kind != model.ToolResult {
				continue
			}
			if _, ok := byCall[b.CallID]; !ok {
				byCall[b.CallID] = ToolResultBody{CallID: b.CallID, Result: tool.Result{Text: b.Result, IsError: b.IsError}, NotMade: b.IsError && b.Result == OneAtATime}
			}
		}
	}
	var lines []string
	for _, m := range append(copied(r.Paused.Before), r.Paused.Reply) {
		if m.Role != model.Assistant {
			continue
		}
		for _, b := range m.Blocks {
			if b.Kind != model.ToolUse || own[b.Tool] {
				continue
			}
			res, ok := byCall[b.CallID]
			if !ok {
				continue
			}
			lines = append(lines, "- "+CalledLine(CalledBody{Tool: b.Tool, Input: cutInput(b.Input), Outcome: outcome(res, answers[b.CallID])}))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return RecordNote + "\n" + strings.Join(lines, "\n")
}

// copied is msgs, copied, so appending to it never writes into the run's.
func copied(msgs []model.Message) []model.Message {
	return append([]model.Message(nil), msgs...)
}
