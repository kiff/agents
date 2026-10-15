// Package agent is the run loop: the model proposes, tools act, and KIFF
// decides what reaches anything outside. A turn goes model → tool calls →
// tools → model, until the model answers, KIFF holds a call, or the run's cost
// cap is reached. Every step is recorded to the session as it happens, and
// streamed to the caller.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool"
)

// DefaultMaxTurns bounds the model turns of one run when Agent.MaxTurns is 0.
const DefaultMaxTurns = 50

// Agent is what a run needs: who it is, which model at which price, its
// instructions (KIFF's skill included) and its tools.
type Agent struct {
	Tenant string
	ID     string

	Model     model.Model
	ModelID   string
	Price     model.Price
	MaxTokens int64

	// System is the agent's instructions, with KIFF's skill in them.
	System string
	Tools  []tool.Tool

	// MaxCost is the most a run may cost, in millionths of a US dollar. A run
	// that reaches it stops before its next model turn.
	MaxCost int64
	// MaxTurns bounds the model turns of one run; 0 means DefaultMaxTurns.
	MaxTurns int
}

// Check refuses an agent that can't run safely: no model or id, no cost cap,
// or a write tool outside KIFF's gateway.
func (a Agent) Check() error {
	switch {
	case a.Tenant == "" || a.ID == "":
		return errors.New("agent: tenant and id are required")
	case a.Model == nil || a.ModelID == "":
		return errors.New("agent: a model and its id are required")
	case a.MaxTokens <= 0:
		return errors.New("agent: max tokens must be positive")
	case a.MaxCost <= 0:
		return errors.New("agent: a run needs a cost cap")
	}
	return tool.Check(a.Tools)
}

// TextBody is the body of OwnerMessage, AgentMessage and Report events.
type TextBody struct {
	Text string `json:"text"`
}

// ToolCallBody is the body of a ToolCall event. The input is recorded as the
// model wrote it; secrets never reach it (keys live in the tool adapters).
type ToolCallBody struct {
	CallID string          `json:"call_id"`
	Tool   string          `json:"tool"`
	Input  json.RawMessage `json:"input"`
}

// ToolResultBody is the body of a ToolResult event.
type ToolResultBody struct {
	CallID string `json:"call_id"`
	Tool   string `json:"tool"`
	tool.Result
}

// ErrorBody is the body of an Error event.
type ErrorBody struct {
	Message string `json:"message"`
}

// EventKind is what a streamed Event says.
type EventKind string

const (
	TextDelta  EventKind = "text"        // more of the agent's answer
	CallStart  EventKind = "tool_call"   // the agent calls a tool
	CallResult EventKind = "tool_result" // a tool answered
)

// Event is streamed to the caller as the run goes.
type Event struct {
	Kind   EventKind       `json:"kind"`
	Text   string          `json:"text,omitempty"`
	CallID string          `json:"call_id,omitempty"`
	Tool   string          `json:"tool,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"`
	Result *tool.Result    `json:"result,omitempty"`
}

// Stop is why a run ended.
type Stop string

const (
	Answered Stop = "answered"  // the agent answered and called nothing more
	Held     Stop = "held"      // KIFF holds a call for the owner; the agent said what it waits for
	CostCap  Stop = "cost_cap"  // the run reached its cost cap
	TurnCap  Stop = "turn_cap"  // the run reached its turn limit
	TooLong  Stop = "max_token" // the model ran out of room mid-answer
)

// Outcome is how a run ended.
type Outcome struct {
	Stop Stop `json:"stop"`
	// Text is the agent's last answer.
	Text string `json:"text"`
	// Cost is what the run's model turns cost, in millionths of a US dollar.
	Cost int64 `json:"cost_micro_usd"`
	// Held are the calls KIFF holds for the owner.
	Held []ToolResultBody `json:"held,omitempty"`
}

// Run runs the agent on input, after history, recording into rec. It returns
// how the run ended and the conversation with this run's turns added, for a
// chat to continue. It doesn't end rec: the caller does, with the status the
// run deserves.
//
// When KIFF holds a call, the agent gets one more turn with no tools, to say
// what it waits for; it can't retry around the hold.
func Run(ctx context.Context, a Agent, rec *session.Recording, history []model.Message, input string, emit func(Event)) (Outcome, []model.Message, error) {
	if err := a.Check(); err != nil {
		return Outcome{}, history, err
	}
	if emit == nil {
		emit = func(Event) {}
	}
	maxTurns := a.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}
	byName := make(map[string]tool.Tool, len(a.Tools))
	specs := make([]model.ToolSpec, 0, len(a.Tools))
	for _, t := range a.Tools {
		byName[t.Spec().Name] = t
		specs = append(specs, t.Spec())
	}
	report := rec.Session().Kind == session.KindRun

	msgs := append(append([]model.Message(nil), history...), model.Message{Role: model.User, Blocks: []model.Block{{Kind: model.Text, Text: input}}})
	if err := rec.Record(session.OwnerMessage, TextBody{Text: input}); err != nil {
		return Outcome{}, msgs, err
	}
	var out Outcome
	final := false // a call is held: one last turn, without tools
	for turn := 0; ; turn++ {
		if out.Cost >= a.MaxCost {
			out.Stop = CostCap
			return out, msgs, nil
		}
		if turn >= maxTurns {
			out.Stop = TurnCap
			return out, msgs, nil
		}
		req := model.Request{Model: a.ModelID, System: a.System, Messages: msgs, MaxTokens: a.MaxTokens}
		if !final {
			req.Tools = specs
		}
		reply, err := a.Model.Turn(ctx, req, func(s string) { emit(Event{Kind: TextDelta, Text: s}) })
		if err != nil {
			_ = rec.Record(session.Error, ErrorBody{Message: err.Error()})
			return out, msgs, err
		}
		cost := a.Price.Cost(reply.Usage)
		out.Cost += cost
		if err := rec.Record(session.Usage, session.UsageBody{Model: a.ModelID, InputTokens: reply.Usage.InputTokens,
			OutputTokens: reply.Usage.OutputTokens, CacheRead: reply.Usage.CacheRead, CacheWrite: reply.Usage.CacheWrite, CostMicroUSD: cost}); err != nil {
			return out, msgs, err
		}
		msgs = append(msgs, model.Message{Role: model.Assistant, Blocks: reply.Blocks})

		var text []string
		var calls []model.Block
		for _, b := range reply.Blocks {
			switch b.Kind {
			case model.Text:
				text = append(text, b.Text)
			case model.ToolUse:
				calls = append(calls, b)
			}
		}
		out.Text = strings.TrimSpace(strings.Join(text, "\n"))
		done := final || len(calls) == 0
		if out.Text != "" {
			kind := session.AgentMessage
			if done && report {
				kind = session.Report
			}
			if err := rec.Record(kind, TextBody{Text: out.Text}); err != nil {
				return out, msgs, err
			}
		}
		if done {
			switch {
			case final:
				out.Stop = Held
			case reply.Stop == model.MaxTokens:
				out.Stop = TooLong
			default:
				out.Stop = Answered
			}
			return out, msgs, nil
		}

		results, err := a.callTools(ctx, rec, byName, calls, emit)
		if err != nil {
			return out, msgs, err
		}
		blocks := make([]model.Block, len(results))
		for i, r := range results {
			blocks[i] = model.Block{Kind: model.ToolResult, CallID: r.CallID, Result: r.Text, IsError: r.IsError}
			if r.Held() {
				out.Held = append(out.Held, r)
				final = true
			}
		}
		msgs = append(msgs, model.Message{Role: model.User, Blocks: blocks})
	}
}

// callTools records the calls in order, runs them in parallel, and records
// their results in the order of the calls.
func (a Agent) callTools(ctx context.Context, rec *session.Recording, byName map[string]tool.Tool, calls []model.Block, emit func(Event)) ([]ToolResultBody, error) {
	for _, c := range calls {
		if err := rec.Record(session.ToolCall, ToolCallBody{CallID: c.CallID, Tool: c.Tool, Input: c.Input}); err != nil {
			return nil, err
		}
		emit(Event{Kind: CallStart, CallID: c.CallID, Tool: c.Tool, Input: c.Input})
	}
	results := make([]ToolResultBody, len(calls))
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = ToolResultBody{CallID: c.CallID, Tool: c.Tool, Result: call(ctx, byName[c.Tool], c)}
		}()
	}
	wg.Wait()
	for _, r := range results {
		if err := rec.Record(session.ToolResult, r); err != nil {
			return nil, err
		}
		emit(Event{Kind: CallResult, CallID: r.CallID, Tool: r.Tool, Result: &r.Result})
	}
	return results, nil
}

// call runs one tool. A missing tool or a failed call is an error result the
// model reads; it never ends the run.
func call(ctx context.Context, t tool.Tool, c model.Block) tool.Result {
	if t == nil {
		return tool.Result{Text: fmt.Sprintf("there is no tool named %q", c.Tool), IsError: true}
	}
	r, err := t.Call(ctx, c.Input)
	if err != nil {
		return tool.Result{Text: "the call failed, and whether it took effect is unknown: " + err.Error(), IsError: true}
	}
	return r
}
