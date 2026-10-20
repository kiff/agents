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
)

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
	// Trial is KIFF's decision on a trial's write (kiff/tafs#107): what it
	// would have done with the call, which it never sent. Set only in a
	// trial run, for a gateway call KIFF decided.
	Trial *TrialDecision `json:"trial,omitempty"`
	// Component is a UI component the tool shows the owner (the show tool,
	// kiff/agents#91). The run records it as its own event, kept like the
	// agent's messages; it is not part of the tool's recorded result.
	Component json.RawMessage `json:"-"`
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

// Shower is a tool whose results may carry a component for the owner's
// chat. Only a Shower's component is recorded: the owner sees it as coming
// from the agent, and the show tool checks each against contract 1.
type Shower interface {
	Shows() bool
}

// Held reports whether KIFF holds the call for its owner.
func (r Result) Held() bool { return r.State == StateHeld }

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
