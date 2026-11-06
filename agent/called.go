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
	// Reason is what the call's result said when it wasn't done (the
	// vendor's refusal as KIFF passed it on, or KIFF's own words), on one
	// line, at most CalledReasonMax. It outlives the result, which a run
	// drops when it ends, so a later run, and Coach, can say why a call
	// failed (kiff/tafs#476, gap 10: "my tool results from earlier runs
	// aren't kept").
	Reason string `json:"reason,omitempty"`
}

// CalledReasonMax is the most of a failed call's result a Called event
// keeps, in characters: a reason, never a payload.
const CalledReasonMax = 300

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
	OutcomeChanged  = "held by KIFF, its Card changed for it by the owner, then sent once: done"
	OutcomeCame     = "sent; its outcome came later, as the vendor's event"
	OutcomeNotCame  = "sent; its outcome didn't come in time (the wait for the vendor's event ran out)"
)

// RecordNote follows a continued run's held calls' results: what the run did,
// from its record. The model says what was done from it, never from its
// memory of the chat. It's for the model: the owner never sees runs or
// records, and a read of the item in an earlier message (its Called line in
// the conversation) is the look-up as much as one in this run; the note
// itself never carries CalledMark, which only the runner's lines do. Told to
// report "from this record, with its amounts and ids, not from earlier
// messages", a taf opened with "This run made one call: a €7.00 (700 cents)
// refund on charge ch_…" and said the look-up made in the message before
// "isn't in this run's record", which the owner reads as unchecked (Eval
// 42146041790). Told then "if you mention it, say it was checked earlier", it
// gave the look-up a sentence of its own ("I had checked the charge earlier
// in this conversation: …"), which a judge fails as narration. After a hold
// the owner answered, the check made before asking gets no word, as kiff/tafs'
// chat rule says (Eval 42176052141, kiff/tafs#560).
const RecordNote = "What this run did, from its record, call by call, in order. " +
	"It's for you, not the owner, who never sees runs or records: tell them what was done, in your rules' words, and never what a run or a record holds. " +
	"Say what was done from this record, not from what earlier messages said would be done. " +
	"A line that says \"looked up\" is this run reading that item before it asked for anything: it is the look-up your rules ask for, made. " +
	"So is a read of the same item earlier in this conversation, made in an earlier message. " +
	"Neither gets a word in your answer: it was made before the owner answered. " +
	"Never say a step was skipped, or an item went unchecked, that this record or the conversation shows was made."

// outcome is what came of a call, from its result. answer is KIFF's answer
// to its hold, when it was held and has been answered. The result decides:
// a hold KIFF answered with another word than approved can still have been
// sent (changed: the Card changed, and the call went under it), so the word
// only says why a call that didn't go through didn't.
func outcome(r ToolResultBody, answer string) string {
	switch {
	case r.NotMade:
		return OutcomeNotMade
	case r.Held():
		return OutcomeHeld
	case r.State == tool.StateRefused && answer != "" && answer != "approved":
		return "held by KIFF, " + answer + ", not sent"
	case r.State == tool.StateRefused:
		return OutcomeRefused
	case r.State == "unknown":
		return "sent, but whether it took effect is unknown"
	case r.IsError:
		return OutcomeFailed
	case answer == "changed":
		return OutcomeChanged
	case answer != "":
		return OutcomeApproved
	}
	return OutcomeDone
}

// recordCalled records a call to a tool outside the agent and its outcome,
// with why it wasn't done when its result says.
func recordCalled(rec *session.Recording, t tool.Tool, name string, input json.RawMessage, oc, reason string) error {
	if t != nil && t.Access() == tool.Own {
		return nil
	}
	return rec.Record(session.Called, CalledBody{Tool: name, Input: cutInput(input), Outcome: oc, Reason: reason})
}

// reasonOf is why a call wasn't done, from its result, for its Called
// event: the result's words when it's an error that was sent or refused,
// on one line and cut to CalledReasonMax; "" for a call that was done, held,
// or not made (its outcome says why).
func reasonOf(r ToolResultBody) string {
	if !r.IsError || r.NotMade || r.Held() {
		return ""
	}
	text := strings.Join(strings.Fields(r.Text), " ")
	if c := []rune(text); len(c) > CalledReasonMax {
		text = strings.TrimSpace(string(c[:CalledReasonMax-1])) + "…"
	}
	return text
}

// OwnRefusedBody is the body of an OwnRefused event: a call to one of the
// agent's own tools that the tool refused, the start of its input, and the
// tool's words for why, as the model read them.
type OwnRefusedBody struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input,omitempty"`
	Error string          `json:"error"`
}

// recordOwnRefused records a call to one of the agent's own tools that came
// back an error, so a transcript names why (a card show refused).
func recordOwnRefused(rec *session.Recording, t tool.Tool, name string, input json.RawMessage, r tool.Result) error {
	if t == nil || t.Access() != tool.Own || !r.IsError {
		return nil
	}
	return rec.Record(session.OwnRefused, OwnRefusedBody{Tool: name, Input: cutInput(input), Error: r.Text})
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

// AnsweredMark opens the line of a card's answer in the chat's history,
// and only that: the model trusts what follows it as the owner's answer.
const AnsweredMark = "[answered]"

// forgedMark is either mark in any spelling the model would read as it.
var forgedMark = regexp.MustCompile(`(?i)\[\s*(called|answered)\s*\]`)

// Unforged is text the runner didn't write as a record (an owner's message,
// a tool's result, a customer's email, the agent's own words) with the
// record's marks taken out, so nothing that only looks like a line of the
// record, or a card's answer, is read as one.
func Unforged(text string) string {
	return forgedMark.ReplaceAllStringFunc(text, func(m string) string {
		return "(" + strings.ToLower(strings.Trim(m, "[] \t")) + ")"
	})
}

// AnsweredBody is the body of an Answered event: the card (its id and type),
// the owner's answer as the app sent it, and their choice in words. Wrote
// is set instead when the owner wrote a message rather than answer it;
// Unanswered, when the card stopped waiting with no answer.
type AnsweredBody struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Answer     json.RawMessage `json:"answer,omitempty"`
	Words      string          `json:"words,omitempty"`
	Wrote      string          `json:"wrote,omitempty"`
	Unanswered bool            `json:"unanswered,omitempty"`
	// AnsweredBy names the service that answered in the owner's place (a
	// result's hand-over); empty when the owner answered.
	AnsweredBy string `json:"answered_by,omitempty"`
}

// AnsweredLine is an Answered event as the model reads it in the chat's
// history: the only text that carries AnsweredMark.
func AnsweredLine(b AnsweredBody) string {
	line := AnsweredMark + " " + Unforged(b.Type) + " " + Unforged(b.ID) + ": "
	switch {
	case b.Unanswered:
		return line + "no answer"
	case b.Wrote != "":
		return line + "the owner wrote instead: " + Unforged(b.Wrote)
	case b.AnsweredBy != "":
		return line + Unforged(b.Words) + " (answered by " + Unforged(b.AnsweredBy) + ", not the owner)"
	}
	return line + Unforged(b.Words)
}

// CalledLine is a Called event as the model reads it: the only text that
// carries CalledMark. A failed call's reason is the result's words, quoted
// as its answer: data, like any tool's result.
func CalledLine(b CalledBody) string {
	line := CalledMark + " " + Unforged(b.Tool)
	if len(b.Input) > 0 {
		line += " " + Unforged(string(b.Input))
	}
	line += " → " + b.Outcome
	if b.Reason != "" {
		line += "; its answer: " + Unforged(b.Reason)
	}
	return line
}

// NoteLine is a call as the record note says it: a read named as the
// look-up it is ("looked up"), a write as what it asked to change, in the
// words the job's rules use, so the model can't take a read it made for a
// check it skipped.
func NoteLine(b CalledBody, read bool) string {
	verb := "asked to change: "
	if read {
		verb = "looked up: "
	}
	line := CalledMark + " " + verb + Unforged(b.Tool)
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
	own, reads := map[string]bool{}, map[string]bool{}
	for _, t := range a.Tools {
		switch t.Access() {
		case tool.Own:
			own[t.Spec().Name] = true
		case tool.Read:
			reads[t.Spec().Name] = true
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
			lines = append(lines, "- "+NoteLine(CalledBody{Tool: b.Tool, Input: cutInput(b.Input), Outcome: outcome(res, answers[b.CallID])}, reads[b.Tool]))
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

// recordAnswered records the owner's answer to the card q waited for, as
// recorded on the run: an Answered event, and, when they wrote a message
// instead, that message as theirs.
func recordAnswered(rec *session.Recording, q run.Requirement) error {
	var card struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	_ = json.Unmarshal(q.Input, &card)
	b := AnsweredBody{ID: card.ID, Type: card.Type}
	var res struct {
		Answer     json.RawMessage `json:"answer"`
		Words      string          `json:"words"`
		Wrote      string          `json:"wrote"`
		AnsweredBy string          `json:"answered_by"`
	}
	switch {
	case q.Answered.IsZero() || json.Unmarshal(q.Result, &res) != nil:
		b.Unanswered = true
	case res.Wrote != "":
		b.Wrote = res.Wrote
		if err := rec.Record(session.OwnerMessage, TextBody{Text: res.Wrote}); err != nil {
			return err
		}
	default:
		b.Answer, b.Words, b.AnsweredBy = res.Answer, res.Words, res.AnsweredBy
	}
	return rec.Record(session.Answered, b)
}
