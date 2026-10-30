package kiff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/run"
)

// OperationArg is the argument the gateway reads a call's operation ID from.
const OperationArg = run.OperationArg

// ErrNotFinal means KIFF has no final outcome for a call yet: the gateway
// answered its repeat with retry "same_call" (it still holds the call, or is
// still deciding or sending it).
var ErrNotFinal = errors.New("kiff: the call has no final outcome yet")

// Collect gets the outcome of each call a paused run waits for: it repeats
// the identical call through the gateway with the call's kiff_operation_id.
// An approved call is sent by KIFF once, and its result comes back; a refused,
// expired or withdrawn one comes back refused. Repeating a call already
// collected returns the same outcome without sending it again, so Collect
// can be retried. It is runner.Runner's Collect.
//
// A call with no final outcome yet (retry "same_call": held, deciding or
// forwarding) is ErrNotFinal, and nothing is returned: the run stays paused,
// and a later Collect gets the outcome.
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
		res, m, err := t.callMeta(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("kiff: collecting call %s: %w", q.CallID, err)
		}
		if m.Retry == "same_call" || res.Held() { // held says so on its own too
			return nil, fmt.Errorf("%w: call %s is %s", ErrNotFinal, q.CallID, m.State)
		}
		out = append(out, agent.ToolResultBody{CallID: q.CallID, Tool: q.Tool, Result: res})
	}
	return out, nil
}

// find is the agent's KIFF gateway tool named name, or nil.
func find(a agent.Agent, name string) *gatewayTool {
	for _, t := range a.Tools {
		if g, ok := t.(*gatewayTool); ok && g.spec.Name == name {
			return g
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
