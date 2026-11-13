package agent

import (
	"encoding/json"
	"strings"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/tool"
)

// After a refused call, a run stops working around it, whatever its
// instructions say (13 Nov: a taf's search of its spreadsheets was refused
// for want of a scope, so it made a new sheet of the same name, and would
// have every week). Two rules hold for every call to an app through KIFF's
// gateway:
//
//   - A read that failed, and hasn't been read again since, means the run
//     doesn't know what's there: nothing is created or changed in that run
//     (AfterRefusedRead). A later read of the same tool that answers lifts it.
//     A vendor's plain "not found" (404) is an answer, not a refusal: a
//     "find it, else create it" run may create. Some apps answer 404 for
//     what the sign-in can't see, too; a kept id, and the app's guide, are
//     the guard there.
//   - A tool gets one more try once it failed, to find out why: failing
//     twice in a run, it isn't called again in that run (AfterTwoFailures).
//
// Each such call is refused back to the model unmade, as OneAtATime is, and
// its words ask the model to stop and tell the person in plain words what
// was refused and what they can do. The agent's own tools (its notes, its
// cards, planque's requests) aren't calls to an app, and don't count.

// AfterRefusedRead is what a call that acts gets back, unmade, while a read
// of the run is still refused.
const AfterRefusedRead = "Not made: a read in this run was refused, so what's there isn't known, and nothing is created or changed in this run. " +
	"Stop here, and tell the person in plain words what couldn't be read and what they can do about it. Don't try another way round."

// AfterTwoFailures is what a call to a tool that already failed twice in
// the run gets back, unmade.
const AfterTwoFailures = "Not made: this tool already failed twice in this run, and it isn't tried again in it. " +
	"Stop here, and tell the person in plain words what was refused and what they can do about it. Don't try another way round."

// OutcomeStopped is a Called event's outcome for a call refused back by
// those rules: never made, and counted nowhere.
const OutcomeStopped = "not made: the run stopped after a refused call"

// stoppedText says text is one of those rules' words.
func stoppedText(text string) bool {
	return text == AfterRefusedRead || text == AfterTwoFailures
}

// stop is what a run knows of its calls' failures: per tool, how many
// failed, and the reads still refused.
type stop struct {
	failed  map[string]int
	refused map[string]bool
}

func newStop() *stop { return &stop{failed: map[string]int{}, refused: map[string]bool{}} }

// failure says a call's result is a failure that counts: a call to an app
// through KIFF's gateway that came back an error, or that KIFF refused. A
// call held for the owner, waiting for its outcome, never made, or in a
// trial didn't fail.
func failure(t tool.Tool, r tool.Result, notMade bool) bool {
	if t == nil || !t.Gateway() || notMade || r.Trial != nil {
		return false
	}
	switch r.State {
	case tool.StateHeld, tool.StateAwaiting, tool.StateWaiting:
		return false
	case tool.StateRefused:
		return true
	}
	return r.IsError
}

// see notes a call's result.
func (s *stop) see(t tool.Tool, name string, input json.RawMessage, r tool.Result, notMade bool) {
	if t == nil || !t.Gateway() || notMade {
		return
	}
	failed := failure(t, r, notMade)
	if failed {
		s.failed[name]++
	}
	// A vendor's "not found" is an answer: what's there is known, it isn't.
	// It still counts toward the tool's two failures. planque words every
	// vendor error as "… refused it (it answered NNN)".
	notFound := failed && r.State != tool.StateRefused && strings.Contains(r.Text, "(it answered 404)")
	if !acts(t, input) {
		switch {
		case failed && !notFound:
			s.refused[name] = true
		case r.State != tool.StateHeld && r.State != tool.StateAwaiting && r.State != tool.StateWaiting:
			delete(s.refused, name)
		}
	}
}

// check is the words a call gets back unmade, or "" when it's made.
func (s *stop) check(t tool.Tool, name string, input json.RawMessage) string {
	if t == nil || !t.Gateway() {
		return ""
	}
	if s.failed[name] >= 2 {
		return AfterTwoFailures
	}
	if acts(t, input) && len(s.refused) > 0 {
		return AfterRefusedRead
	}
	return ""
}

// stopIn is what this run's own messages say of its failures, for a run
// resumed from them: its tool calls since the run's history, and their
// results. A result whose record didn't keep its state counts by its error.
func (a Agent) stopIn(msgs []model.Message, byName map[string]tool.Tool) *stop {
	s := newStop()
	if a.history > len(msgs) {
		return s
	}
	calls := map[string]model.Block{}
	for _, m := range msgs[a.history:] {
		for _, b := range m.Blocks {
			switch b.Kind {
			case model.ToolUse:
				calls[b.CallID] = b
			case model.ToolResult:
				c, ok := calls[b.CallID]
				if !ok {
					continue
				}
				notMade := b.IsError && (b.Result == OneAtATime || stoppedText(b.Result))
				r := tool.Result{Text: b.Result, IsError: b.IsError}
				if oc, ok := a.resolved[b.CallID]; ok && !strings.HasPrefix(oc, "failed") && oc != OutcomeRefused {
					// Held, then answered: what came of it is the outcome, not an error.
					r.IsError = false
				}
				s.see(byName[c.Tool], c.Tool, c.Input, r, notMade)
			}
		}
	}
	return s
}
