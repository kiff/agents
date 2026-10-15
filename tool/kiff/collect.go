package kiff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/tool"
)

// OperationArg is the argument the gateway reads a call's operation ID from.
const OperationArg = "kiff_operation_id"

// ErrStillHeld means KIFF still holds a call: the owner hasn't answered, and
// its hold hasn't expired.
var ErrStillHeld = errors.New("kiff: the call is still held")

// Collect gets the outcome of each call a paused run waits for: it repeats
// the identical call through the gateway with the call's kiff_operation_id.
// An approved call is sent by KIFF once, and its result comes back; a refused,
// expired or withdrawn one comes back refused. Repeating a call already
// collected returns the same outcome without sending it again, so Collect
// can be retried. It is runner.Runner's Collect.
//
// A call KIFF still holds is ErrStillHeld, and nothing is returned: the run
// stays paused.
func Collect(ctx context.Context, a agent.Agent, held []run.Requirement) ([]agent.ToolResultBody, error) {
	out := make([]agent.ToolResultBody, 0, len(held))
	for _, q := range held {
		if q.Operation == "" {
			return nil, fmt.Errorf("kiff: call %s has no operation ID to collect it by", q.CallID)
		}
		t := find(a, q.Tool)
		if t == nil {
			return nil, fmt.Errorf("kiff: call %s: the agent has no gateway tool %q", q.CallID, q.Tool)
		}
		input, err := withOperation(q.Input, q.Operation)
		if err != nil {
			return nil, fmt.Errorf("kiff: call %s: %w", q.CallID, err)
		}
		res, err := t.Call(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("kiff: collecting call %s: %w", q.CallID, err)
		}
		if res.Held() {
			return nil, fmt.Errorf("%w: call %s", ErrStillHeld, q.CallID)
		}
		out = append(out, agent.ToolResultBody{CallID: q.CallID, Tool: q.Tool, Result: res})
	}
	return out, nil
}

// find is the agent's gateway tool named name, or nil.
func find(a agent.Agent, name string) tool.Tool {
	for _, t := range a.Tools {
		if t.Gateway() && t.Spec().Name == name {
			return t
		}
	}
	return nil
}

// withOperation is a call's input, a JSON object, with its operation ID set.
func withOperation(input json.RawMessage, op string) (json.RawMessage, error) {
	args := map[string]json.RawMessage{}
	if len(input) > 0 && string(input) != "null" {
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, fmt.Errorf("its input is not a JSON object: %w", err)
		}
	}
	v, err := json.Marshal(op)
	if err != nil {
		return nil, err
	}
	args[OperationArg] = v
	return json.Marshal(args)
}
