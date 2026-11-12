package agent

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/tool"
)

// The count of a run's calls that act (run.Tally). A model that totals its
// calls in its head gets the total wrong: a taf's report gave a number of calls
// its record didn't show (Eval 42788659962), and a rule that told it to count
// the calls its record shows left the counting to the same head. So the runner counts, from each call's outcome as its
// Called event has it, and says the count after the results of every turn
// that acted: the number the model reports is one it read, not one it made.

// Shown the count, a model still wrote its own: "I submitted evidence on six
// disputes" with five sent and one held, and "none were refused" with two
// reads refused that the count left out (Eval 43243736555). So a refused read
// is counted too (counts), the run keeps its count (run.Run.Tally) for the
// caller to show beside the model's words, and the rule no longer asks the
// model to copy a number: it asks it to give none.

// TallyNote opens the count the model reads after the results of a turn
// that acted, and TallyRule closes it.
const (
	TallyNote = "Count so far of this run's calls that act, and of those KIFF refused, from its record: "
	TallyRule = " This count is shown beside your words from the record, so never write a number of what was sent, held, refused or failed, whether calls or the items they acted on (\"six disputes\"), nor that none were: say what was done and what waits, in words."
)

// counts is whether a call goes in the count: one that acts, and one KIFF
// refused, a read included. A refused read changed nothing, but it is a call
// that didn't go through, and a report saying "none were refused" would be
// wrong without it.
func counts(t tool.Tool, input json.RawMessage, outcome string) bool {
	if acts(t, input) {
		return true
	}
	return TallyOf(outcome) == "refused" && (t == nil || t.Access() != tool.Own)
}

// TallyOf is the count a call's outcome, as its Called event has it, adds
// to: one of sent, held, refused, failed or unknown, and "" for a call that
// was never made (OutcomeNotMade).
func TallyOf(outcome string) string {
	switch {
	case outcome == OutcomeDone, outcome == OutcomeApproved, outcome == OutcomeChanged, outcome == OutcomeCame:
		return "sent"
	case outcome == OutcomeHeld:
		return "held"
	case outcome == OutcomeRefused, strings.HasPrefix(outcome, "held by KIFF, ") && strings.HasSuffix(outcome, ", not sent"):
		return "refused"
	case outcome == OutcomeFailed:
		return "failed"
	case outcome == OutcomeNotMade:
		return ""
	}
	// Sent with no word on what came of it (the vendor's event didn't come
	// in time, or the answer was lost), and any outcome named since.
	return "unknown"
}

// Counted is t with one call of an outcome added.
func Counted(t run.Tally, outcome string) run.Tally {
	switch TallyOf(outcome) {
	case "sent":
		t.Sent++
	case "held":
		t.Held++
	case "refused":
		t.Refused++
	case "failed":
		t.Failed++
	case "unknown":
		t.Unknown++
	}
	return t
}

// tally is a run's count, by tool.
type tally map[string]run.Tally

// add counts one call of tool with an outcome.
func (t tally) add(tool, outcome string) {
	if n := Counted(t[tool], outcome); n != t[tool] {
		t[tool] = n
	}
}

// with is t and more, tool by tool; t and more stay as they were.
func (t tally) with(more tally) tally {
	out := maps.Clone(t)
	if out == nil {
		out = tally{}
	}
	for name, m := range more {
		n := out[name]
		n.Sent, n.Held, n.Refused, n.Failed, n.Unknown = n.Sent+m.Sent, n.Held+m.Held, n.Refused+m.Refused, n.Failed+m.Failed, n.Unknown+m.Unknown
		out[name] = n
	}
	return out
}

// TallyLine is the count as the model reads it: each tool by name, in
// order, with its four counts, and the calls whose outcome isn't known only
// when there are some. "" when nothing was counted.
func TallyLine(t map[string]run.Tally) string {
	if len(t) == 0 {
		return ""
	}
	parts := make([]string, 0, len(t))
	for _, name := range slices.Sorted(maps.Keys(t)) {
		n := t[name]
		part := fmt.Sprintf("%s: %d sent, %d held, %d refused, %d failed", Unforged(name), n.Sent, n.Held, n.Refused, n.Failed)
		if n.Unknown > 0 {
			part += fmt.Sprintf(", %d sent with no word on whether it took effect", n.Unknown)
		}
		parts = append(parts, part)
	}
	return TallyNote + strings.Join(parts, "; ") + "." + TallyRule
}
