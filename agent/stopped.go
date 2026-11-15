package agent

import (
	"encoding/json"
	"fmt"
	"strconv"
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
//     doesn't know what's there: nothing it asked about is created or
//     changed in that run (AfterRefusedRead). A later read of the same tool
//     that answers lifts it. What it asked about is the items it named
//     (itemsOf): a write that names one of them, or any write after a read
//     that named none (a search, or a list, filtered or not), waits. A read of one item
//     that failed on a wrong id says nothing of another item (Eval
//     43695849959's handoff-receiver: two reads with mistyped dispute ids
//     failed, and the evidence on the right dispute was never submitted).
//     A read KIFF refused outside the Card isn't one: that's the owner's
//     limit, not a sign of what the app holds, so the run's other work goes
//     on (13 Nov: a chargebacks taf whose Card leaves out the shop's order
//     lookup stopped settling its disputes).
//   - A call gets one more try once it failed, to find out why: failing
//     twice in a run, it isn't made again in that run (AfterTwoFailures).
//     An app's error counts per call, the tool with its input: a batch
//     job's typo in one item's id stopped every other item's write (Eval
//     43674391351, chargebacks run 2: two update_dispute calls failed on a
//     mistyped dispute id, and the other disputes went unsettled). A tool
//     that errs on every call still stops, after FailuresPerTool errors in
//     all. A call KIFF refused counts per tool: the owner's limit is the
//     tool's, whatever the input.
//
// Each such call is refused back to the model unmade, as OneAtATime is, and
// its words ask the model to stop and tell the person in plain words what
// was refused and what they can do. The agent's own tools (its notes, its
// cards, planque's requests) aren't calls to an app, and don't count.

// AfterRefusedRead is what a call that acts gets back, unmade, while a read
// of the run is still refused.
const AfterRefusedRead = "Not made: a read in this run was refused, so what's there isn't known, and nothing is created or changed in this run. " +
	"Stop here, and tell the person in plain words what couldn't be read and what they can do about it. Don't try another way round."

// AfterTwoFailures is what the same call, already failed twice in the run
// by the app, gets back, unmade. It says the call, not the tool: other calls
// to the tool still go, and a taf told "this tool" stopped would say so to
// its owner, and be judged against it (dev-android, on this rule's move to
// counting per call).
const AfterTwoFailures = "Not made: this call already failed twice in this run, and it isn't tried again in it. " +
	"Don't try it another way: tell the person in plain words what failed and what they can do about it."

// AfterToolFailures is what a call to a tool the app failed on
// FailuresPerTool calls in the run, on any inputs, gets back, unmade: no call
// to it is made again in the run.
var AfterToolFailures = fmt.Sprintf("Not made: this tool failed on %d calls in this run, and it isn't called again in it. ", FailuresPerTool) +
	"Stop here, and tell the person in plain words what failed and what they can do about it. Don't try another way round."

// AfterTwoRefusals is what a call to a tool KIFF refused twice in the run
// gets back, unmade: the owner's limit is the tool's, so no call to it is
// made again in the run.
const AfterTwoRefusals = "Not made: KIFF refused this tool twice in this run, and it isn't called again in it. " +
	"Stop here, and tell the person in plain words what was refused and what they can do about it. Don't try another way round."

// FailuresPerTool is how many of a tool's calls an app may fail in a run, on
// any inputs, before the tool isn't called again in it: two items' tries
// each and one more, so an app that errs on every call (its sign-in ended,
// say) stops the run as before, a few calls later.
const FailuresPerTool = 5

// OutcomeStopped is a Called event's outcome for a call refused back by
// those rules: never made, and counted nowhere.
const OutcomeStopped = "not made: the run stopped after a refused call"

// stoppedText says text is one of those rules' words.
func stoppedText(text string) bool {
	return text == AfterRefusedRead || text == AfterTwoFailures || text == AfterToolFailures || text == AfterTwoRefusals
}

// stop is what a run knows of its calls' failures: per tool, how many KIFF
// refused and how many the app failed; per call (the tool with its input),
// how many the app failed; and the reads still refused.
type stop struct {
	refusedBy map[string]int
	erredBy   map[string]int
	erred     map[string]int
	// refused is, by tool, the items each of its reads that failed asked
	// about (itemsOf), until a read of the tool answers: none for a read
	// that named no item.
	refused map[string][][]string
}

func newStop() *stop {
	return &stop{refusedBy: map[string]int{}, erredBy: map[string]int{}, erred: map[string]int{}, refused: map[string][][]string{}}
}

// itemsOf are the items a read asked about, lowercased: the value KIFF says
// the call is on (resource, its Card's resource argument), and the values of
// the input's keys that name an item (namesItem: "order_id", "orderId",
// "dispute" in retrieve_dispute). Numbers count as their
// text. None for a read of everything, a search or a filtered list: its
// status, query or cursor names no item (dev-android on #502).
func itemsOf(name string, input json.RawMessage, resource string) []string {
	var out []string
	add := func(v any) {
		switch v := v.(type) {
		case string:
			if s := strings.ToLower(strings.TrimSpace(v)); s != "" {
				out = append(out, s)
			}
		case float64:
			out = append(out, strconv.FormatFloat(v, 'f', -1, 64))
		}
	}
	if resource != "" {
		add(resource)
	}
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return r == '_' || r == '.' || r == '-' }) {
		words[w] = true
	}
	var obj map[string]any
	if json.Unmarshal(input, &obj) != nil {
		return out
	}
	for k, v := range obj {
		if !namesItem(k, words) {
			continue
		}
		if list, ok := v.([]any); ok {
			for _, e := range list {
				add(e)
			}
			continue
		}
		add(v)
	}
	return out
}

// namesItem says an input key names an item: "id" or "ids", one ending in
// "_id", "_ids", "-id", or, in camel case, "Id" or "Ids" ("orderId"), or a
// word of the tool's own name. "paid" or "valid" doesn't.
func namesItem(key string, words map[string]bool) bool {
	low := strings.ToLower(key)
	for _, end := range []string{"_id", "_ids", "-id"} {
		if strings.HasSuffix(low, end) {
			return true
		}
	}
	return low == "id" || low == "ids" || strings.HasSuffix(key, "Id") || strings.HasSuffix(key, "Ids") || words[low]
}

// valuesOf are every string and number in a write's input, at any depth,
// lowercased, numbers as their text: what it may change.
func valuesOf(input json.RawMessage) []string {
	var v any
	if json.Unmarshal(input, &v) != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(x any) {
		switch x := x.(type) {
		case string:
			if s := strings.ToLower(strings.TrimSpace(x)); s != "" {
				out = append(out, s)
			}
		case float64:
			out = append(out, strconv.FormatFloat(x, 'f', -1, 64))
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
	return out
}

// blocks says a failed read's items stop a write with these values: a read
// that named no item stops every write; one that did stops a write that
// names one of them anywhere in its input.
func blocks(read, write []string) bool {
	if len(read) == 0 {
		return true
	}
	for _, r := range read {
		for _, w := range write {
			if r == w {
				return true
			}
		}
	}
	return false
}

// callKey is a call's tool and input, the input in one form however its keys
// were ordered or spaced, so the same call made twice is one key.
func callKey(name string, input json.RawMessage) string {
	var v any
	if err := json.Unmarshal(input, &v); err != nil {
		return name + "\x00" + strings.TrimSpace(string(input))
	}
	b, err := json.Marshal(v)
	if err != nil {
		return name + "\x00" + strings.TrimSpace(string(input))
	}
	return name + "\x00" + string(b)
}

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
	switch {
	case failed && r.State == tool.StateRefused:
		s.refusedBy[name]++
	case failed:
		s.erredBy[name]++
		s.erred[callKey(name, input)]++
	}
	if !acts(t, input) {
		switch {
		case failed && r.State == tool.StateRefused:
			// Outside the Card: it says nothing of what's there, and lifts nothing either.
		case failed:
			s.refused[name] = append(s.refused[name], itemsOf(name, input, r.Resource))
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
	if s.refusedBy[name] >= 2 {
		return AfterTwoRefusals
	}
	if s.erredBy[name] >= FailuresPerTool {
		return AfterToolFailures
	}
	if s.erred[callKey(name, input)] >= 2 {
		return AfterTwoFailures
	}
	if acts(t, input) && len(s.refused) > 0 {
		words := valuesOf(input)
		for _, reads := range s.refused {
			for _, read := range reads {
				if blocks(read, words) {
					return AfterRefusedRead
				}
			}
		}
	}
	return ""
}

// stopIn is what this run's own messages say of its failures, for a run
// resumed from them: its tool calls since the run's history, and their
// results. A result keeps its state only when KIFF refused it outside the
// Card (model.Block.State); any other counts by its error.
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
				oc, held := a.resolved[b.CallID]
				if !held {
					// Refused on the Card, not failed: the state kept with the message says so.
					r.State = b.State
				} else if !strings.HasPrefix(oc, "failed") && oc != OutcomeRefused {
					// Held, then answered: what came of it is the outcome, not an error.
					r.IsError = false
				}
				s.see(byName[c.Tool], c.Tool, c.Input, r, notMade)
			}
		}
	}
	return s
}
