package runner

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/tool"
)

// AskTheOwner is what a held call that wasn't approved says the run does
// next: the owner decides again, never the agent on its own.
const AskTheOwner = "Don't make this call again on your own. Tell the owner it wasn't sent and why, and wait: they can ask again if they still want it."

// NotAgain is the result of a call a run makes again after the owner didn't
// approve it (refused, or the hold expired): it isn't sent, and KIFF isn't
// asked again, so a run never turns one request into a chain of holds.
const NotAgain = "Not sent: this same call was held for the owner earlier in this run and wasn't approved (refused, or the hold expired). Only the owner can ask for it again. Tell them, and wait."

// unapproved is each held call of r that the owner didn't approve, by tool
// and input, from what KIFF answered and what collecting it returned.
func unapproved(reqs []run.Requirement, held []agent.ToolResultBody) map[string][]json.RawMessage {
	out := map[string][]json.RawMessage{}
	for _, q := range reqs {
		no := q.Answer == "refused" || q.Answer == "rejected" || q.Answer == "expired"
		if !no && q.Answer == "" {
			for _, h := range held {
				no = no || (h.CallID == q.CallID && h.Result.State == tool.StateRefused)
			}
		}
		if no {
			out[q.Tool] = append(out[q.Tool], q.Input)
		}
	}
	return out
}

// notAgain wraps the run's tools so a call the owner didn't approve isn't
// made again in the same run: the identical call answers NotAgain without
// reaching KIFF. Any other call goes as before.
func notAgain(tools []tool.Tool, refused map[string][]json.RawMessage) []tool.Tool {
	if len(refused) == 0 {
		return tools
	}
	out := make([]tool.Tool, len(tools))
	for i, t := range tools {
		if inputs := refused[t.Spec().Name]; len(inputs) > 0 {
			out[i] = onceRefused{Tool: t, inputs: inputs}
		} else {
			out[i] = t
		}
	}
	return out
}

type onceRefused struct {
	tool.Tool
	inputs []json.RawMessage
}

func (t onceRefused) Call(ctx context.Context, input json.RawMessage) (tool.Result, error) {
	for _, in := range t.inputs {
		if sameJSON(in, input) {
			return tool.Result{Text: NotAgain, IsError: true}, nil
		}
	}
	return t.Tool.Call(ctx, input)
}

// sameJSON reports whether a and b are the same JSON value, whatever their
// spacing or key order.
func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(bytes.TrimSpace(a), bytes.TrimSpace(b))
	}
	ja, _ := json.Marshal(x)
	jb, _ := json.Marshal(y)
	return bytes.Equal(ja, jb)
}
