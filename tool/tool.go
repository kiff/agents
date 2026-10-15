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
