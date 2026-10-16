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

// ErrNotFinal means KIFF has no final outcome for a call yet: it still holds
// it (the owner hasn't answered and the hold hasn't expired), or it is still
// deciding or sending it. The gateway says retry "same_call" for each.
var ErrNotFinal = errors.New("kiff: the call has no final outcome yet")

// notFinal are the gateway's states that carry no outcome: repeating the call
// later gets it.
var notFinal = map[string]bool{"held": true, "deciding": true, "forwarding": true}

// Collect gets the outcome of each call a paused run waits for: it repeats
// the identical call through the gateway with the call's kiff_operation_id.
// An approved call is sent by KIFF once, and its result comes back; a refused,
// expired or withdrawn one comes back refused. Repeating a call already
// collected returns the same outcome without sending it again, so Collect
// can be retried. It is runner.Runner's Collect.
//
// A call with no final outcome yet (held, deciding or forwarding) is
// ErrNotFinal, and nothing is returned: the run stays paused, and a later
// Collect gets the outcome.
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
		if notFinal[res.State] {
			return nil, fmt.Errorf("%w: call %s is %s", ErrNotFinal, q.CallID, res.State)
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
