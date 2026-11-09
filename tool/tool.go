// Package tool is what an agent can call. Every tool says what it can change:
// nothing, only the agent's own memory, or something outside the agent
// (money, access, data, a message to anyone). A tool that changes something
// outside must go through KIFF's gateway, which checks the call against the
// agent's Card first. Check refuses any set of tools that breaks that.
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/kiff/agents/model"
)

// Access is what a tool can change.
type Access int

const (
	// Read changes nothing.
	Read Access = iota
	// Own changes only the agent's own state, such as its memory notes.
	Own
	// Write changes something outside the agent. Only through KIFF's gateway.
	Write
)

// The states KIFF's gateway reports for a call it didn't simply forward
// (its result's _meta "dev.kiff/call").
const (
	StateHeld    = "held"    // waits for the owner's answer
	StateRefused = "refused" // outside the Card, and not held
	// StateAwaiting is a card shown to the owner that waits for their answer
	// (show's answerable types): the run pauses as for a hold, and the
	// answer comes back as the call's result, never as the owner's text.
	StateAwaiting = "awaiting"
	// StateWaiting is a call the connection sent whose outcome comes later,
	// as an event of the vendor's app (the slow-tools design): the run
	// pauses as for a card, and the event, matched by its key, answers it.
	StateWaiting = "waiting"
	// StateFailed is a call KIFF couldn't decide or send: like a held or a
	// refused one, it never reached the vendor. Every other state
	// (forwarded, forwarding, sending, unknown, and deciding, which is "not
	// sent yet": another attempt of the same call may still send it), and a
	// result with no state at all, may have: see MaySend.
	StateFailed = "failed"
)

// MaySend reports whether a call with this result may have reached the
// vendor, by the gateway's word on the call and never by its error mark:
// KIFF marks its own answers as errors, among them a call it sent whose
// answer was lost. Only a held, refused or failed call, and a trial's, is
// known not sent. Anything else counts as sent, a result with no state
// included (a call that went through, or a tool outside KIFF).
func (r Result) MaySend() bool {
	if r.Trial != nil {
		return false
	}
	switch r.State {
	case StateHeld, StateRefused, StateFailed:
		return false
	}
	return true
}

// Result is what a call returned. State is the gateway's word on the call,
// empty for tools that don't go through it.
type Result struct {
	Text    string `json:"text"`
	IsError bool   `json:"is_error,omitempty"`
	State   string `json:"state,omitempty"`
	// Hold names a held call in KIFF: how to collect its outcome, how its
	// answer is announced and withdrawn, and when it stops waiting. Set only
	// when State is StateHeld.
	Hold *Hold `json:"hold,omitempty"`
	// Wait names what a call whose outcome comes later waits for: the
	// event, the key the event carries, and how long. Set only when State
	// is StateWaiting.
	Wait *Wait `json:"wait,omitempty"`
	// Trial is KIFF's decision on a trial's write (kiff/tafs#107): what it
	// would have done with the call, which it never sent. Set only in a
	// trial run, for a gateway call KIFF decided.
	Trial *TrialDecision `json:"trial,omitempty"`
	// Invocation is KIFF's key for a gateway call ("tc-…"), as its result
	// said: the entity KIFF's activity and receipt file the call under, so
	// tafs can find this call from its Record (kiff/tafs#167). Empty for a
	// tool that isn't KIFF's, or a gateway that doesn't say.
	Invocation string `json:"invocation,omitempty"`
	// Resource is what the call acts on (a payment's id), when KIFF names
	// the argument its Card scopes on: kept apart from the result, as the
	// run's call_ref, so the Record can name it after the run (kiff/tafs#167).
	Resource string `json:"-"`
	// Component is a UI component the tool shows the owner (the show tool,
	// kiff/agents#91). The run records it as its own event, kept like the
	// agent's messages; it is not part of the tool's recorded result.
	Component json.RawMessage `json:"-"`
	// Images are what the tool showed besides its text (a screenshot of the
	// app Coach drives, kiff/tafs#544): the model reads them with the text
	// while the run lives; the record keeps the text alone.
	Images []model.Image `json:"-"`
}

// TrialDecision is what KIFF would have done with a trial's write.
type TrialDecision struct {
	// Decision is "allowed", "held" or "refused".
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
	// Title is the tool's name for people, as KIFF keeps it.
	Title string `json:"title,omitempty"`
}

// Hold is KIFF's handle on a held call. A field KIFF didn't send, or sent in
// another shape, is empty: with no Operation the call can't be collected,
// with no Exception it can't be withdrawn, and with no Expires the
// requirement resolves only once answered (it fails closed).
type Hold struct {
	// Operation is the call's kiff_operation_id: repeating the identical
	// call with it collects the outcome, once the owner has answered.
	Operation string `json:"operation"`
	// Exception is KIFF's ID for the owner's decision: what its
	// hold_answered alert names, and what withdrawing the call takes.
	Exception string `json:"exception"`
	// Expires is when the hold stops waiting. Expiry means refused.
	Expires time.Time `json:"expires"`
}

// Wait is what a call whose outcome comes later waits for, as KIFF's
// gateway writes it in the call's _meta (dev.kiff/call.wait, from the
// connection's answer, for the caller's own grant only): the event of the
// app that completes it, the key that event carries (the call's handle),
// the grant the event comes on, how long to wait, and the words the owner
// reads.
type Wait struct {
	Grant    string `json:"grant,omitempty"`
	Event    string `json:"event"`
	Key      string `json:"key"`
	TimeoutS int    `json:"timeout_s"`
	Words    string `json:"words,omitempty"`
}

// Shower is a tool whose results may carry a component for the owner's
// chat. Only a Shower's component is recorded: the owner sees it as coming
// from the agent, and the show tool checks each against contract 1.
type Shower interface {
	Shows() bool
}

// Reader is a tool some of whose calls only read, and others don't (planque's
// status and catalog, against its requests): Reads says which.
type Reader interface {
	Reads(input json.RawMessage) bool
}

// Reads reports whether a call of t with input only reads: every call of a
// Read tool, and a Reader's calls it says so of. What such a call read may be
// kept for a chat's next runs (session.KeptRead); a write's result never is.
func Reads(t Tool, input json.RawMessage) bool {
	if t == nil {
		return false
	}
	if t.Access() == Read {
		return true
	}
	r, ok := t.(Reader)
	return ok && r.Reads(input)
}

// Stater is a tool that holds what it made outside the conversation, such as
// a vendor grant or a KIFF draft. At the start of each run, State says where
// each one the session's components name stands now, from the tool's own
// source, so the agent never depends on remembering it from the chat. shown
// is the session's components, oldest first; an empty answer says nothing.
type Stater interface {
	State(ctx context.Context, shown []json.RawMessage) (string, error)
}

// Named is the distinct string values of field in components, in the order
// they last appear, at most max of the latest: what a Stater looks up.
func Named(components []json.RawMessage, field string, max int) []string {
	var out []string
	for _, c := range components {
		var v map[string]json.RawMessage
		if json.Unmarshal(c, &v) != nil {
			continue
		}
		var s string
		if json.Unmarshal(v[field], &s) != nil || s == "" {
			continue
		}
		out = slices.DeleteFunc(out, func(x string) bool { return x == s })
		out = append(out, s)
	}
	if len(out) > max {
		out = out[len(out)-max:]
	}
	return out
}

// Each runs f for 0..n-1 concurrently, at most limit at once, and returns
// when all have: a Stater's reads of what the session names don't wait on
// one another, and their answers keep their order (f writes its own slot).
func Each(n, limit int, f func(i int)) {
	if limit < 1 {
		limit = 1
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, limit)
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			f(i)
		}()
	}
	wg.Wait()
}

// Held reports whether KIFF holds the call for its owner.
func (r Result) Held() bool { return r.State == StateHeld }

// Waits reports whether the run waits on the call: KIFF holds it, it is a
// card the owner hasn't answered yet, or its outcome comes later as an
// event.
func (r Result) Waits() bool {
	return r.State == StateHeld || r.State == StateAwaiting || r.State == StateWaiting
}

// Tool is one tool an agent can call.
type Tool interface {
	Spec() model.ToolSpec
	Access() Access
	// Gateway reports whether calls go through KIFF's gateway.
	Gateway() bool
	// Call runs the tool. An error means the call couldn't be made or its
	// outcome is unknown; a refusal or a failure the tool reports is a Result
	// with IsError set.
	Call(ctx context.Context, input json.RawMessage) (Result, error)
}

// ResultSchemer is a tool that says what its results hold: the JSON Schema
// of a result (MCP's outputSchema), when the vendor gives one. The agent
// reads which of a result's fields are times by it (agent.dated).
type ResultSchemer interface {
	ResultSchema() json.RawMessage
}

// ErrUngoverned means a tool can change something outside the agent without
// going through KIFF's gateway.
var ErrUngoverned = errors.New("tool: a write tool must go through KIFF's gateway")

// Check refuses a set of tools with a write tool outside the gateway, or two
// tools with one name.
func Check(tools []Tool) error {
	seen := map[string]bool{}
	for _, t := range tools {
		name := t.Spec().Name
		if name == "" {
			return errors.New("tool: a tool has no name")
		}
		if seen[name] {
			return fmt.Errorf("tool: two tools are named %q", name)
		}
		seen[name] = true
		if t.Access() == Write && !t.Gateway() {
			return fmt.Errorf("%w: %s", ErrUngoverned, name)
		}
	}
	return nil
}
