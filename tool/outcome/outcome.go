// Package outcome is the tool a run answers its request with when the caller
// named what it may answer (run.Run.Outcomes): a door from another
// application, whose request ends with one of the door's own outcomes
// ("refunded", "refused", "needs_review"). The value is checked here against
// that list, and the runner keeps the accepted one on the run (run.Run.Outcome):
// kiff/agents' record, never the model's words. It writes nothing outside the
// agent, so it's the agent's own tool: no Card governs it.
package outcome

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/tool"
)

// Name is the tool's name, as the model sees it.
const Name = "request_outcome"

// Tool is one run's outcome, from the list its caller named.
type Tool struct {
	outcomes []string
}

// New returns the tool for a run whose caller named outcomes; nil with none.
func New(outcomes []string) *Tool {
	if len(outcomes) == 0 {
		return nil
	}
	return &Tool{outcomes: slices.Clone(outcomes)}
}

func (t *Tool) Access() tool.Access { return tool.Own }
func (t *Tool) Gateway() bool       { return false }

func (t *Tool) Spec() model.ToolSpec {
	return model.ToolSpec{
		Name: Name,
		Description: "Answer this request with its outcome, once, when the work is done: value is one of " + strings.Join(t.outcomes, ", ") + ". " +
			"It's what the application that sent the request reads back, so it says what happened, never what you hoped: when a call was held or refused, " +
			"answer the outcome that says so. Nothing else answers the request: your words don't.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string","enum":` + mustJSON(t.outcomes) + `}},"required":["value"]}`),
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Accepted is the value a call answers with when it's one of the run's
// outcomes; empty otherwise. The agent keeps the last accepted one.
func (t *Tool) Accepted(input json.RawMessage) string {
	var in struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(input, &in) != nil || !slices.Contains(t.outcomes, in.Value) {
		return ""
	}
	return in.Value
}

// Call accepts one of the run's outcomes, and refuses anything else, saying
// which it may be.
func (t *Tool) Call(_ context.Context, input json.RawMessage) (tool.Result, error) {
	if v := t.Accepted(input); v != "" {
		return tool.Result{Text: fmt.Sprintf("Kept: the request's outcome is %s.", v)}, nil
	}
	return tool.Result{Text: "value is one of " + strings.Join(t.outcomes, ", ") + ": nothing else answers this request", IsError: true}, nil
}
