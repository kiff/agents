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
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kiff/agents/attach"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
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
	// Effort is how hard the model works on each turn; empty is the
	// provider's default.
	Effort string
	// Thinking model.ThinkingSummarized streams a summary of the model's
	// thinking as Thinking events; empty keeps it hidden.
	Thinking string
	// Fast runs the turns at the provider's fast speed, at PriceFast, for a
	// run someone waits on. A turn that couldn't run fast runs at standard,
	// and the rest of the run stays there: the cache is per speed.
	Fast      bool
	PriceFast model.Price

	// System is the agent's instructions, with KIFF's skill in them.
	System string
	Tools  []tool.Tool
	// Deferred are tool name prefixes: a tool whose name starts with one is
	// left out of the prompt until the model searches for it
	// (model.ToolSpec.Defer), so a turn that doesn't need it pays nothing.
	Deferred []string
	// DeferVendor defers every gateway tool that isn't KIFF's own (kiff_*),
	// whatever its name: tools a Card adds mid-conversation then leave the
	// cached prefix as it was.
	DeferVendor bool

	// MaxCost is the most a run may cost, in millionths of a US dollar. A run
	// that reaches it stops before its next model turn.
	MaxCost int64
	// MaxTurns bounds the model turns of one run; 0 means DefaultMaxTurns.
	MaxTurns int

	// Cancel, when set, says whether the run was asked to stop. The loop asks
	// at each checkpoint, before a model turn (so after each tool batch), and
	// a run asked to stop stops there, Cancelled. A turn in which KIFF held a
	// call pauses the run instead: its held calls are withdrawn in KIFF
	// before a paused run is cancelled.
	Cancel func(ctx context.Context) (bool, error)
	// Preface is what the run reads before its input, in the same message:
	// what stands for the session's earlier part, where things stand now,
	// what it read earlier (the runner makes it, kiff/tafs#138). It is
	// never recorded, so each run, and each continuation, reads its own.
	Preface []model.Block
	// Attachments are the files the person sent with the run's input
	// (kiff/tafs#209): they follow it, fenced (attach.FileNote).
	Attachments []attach.Ref
	// Load fills the File blocks of what a run sends with their bytes, once
	// at its start: the newest within the turn budget, the rest out of view.
	// Nil: none are loaded, so every file reads as out of view.
	Load func(context.Context, []model.Message) error
	// Started is how long the run took from its creation to its first model
	// turn, recorded on that turn's Usage; 0 when unknown.
	Started time.Duration
	// Spent is what the run spent before its loop, such as on a summary, in
	// millionths of a US dollar: it counts toward MaxCost.
	Spent int64

	// Steer, when set, takes the messages the owner wrote to the run while it
	// went (#102), and marks them taken. The loop asks at each checkpoint,
	// after Cancel; More asks once the agent has answered, so a message that
	// came while it wrote its answer gets a turn of its own. Each one taken is
	// recorded as an owner message, streamed as Steered, and added to what the
	// model reads next, never inside a tool's result.
	Steer func(ctx context.Context) ([]string, error)

	// Now is the clock a card's wait is counted from (run.CardWait); nil is
	// the system clock.
	Now func() time.Time

	// history is how many of a run's messages are its history (Run, Continue):
	// the prefix the next run renders the same way, cached apart.
	history int
	// resumed says the next turn is the first after Continue: its timing
	// says so (Resumed).
	resumed bool
}

// clock is a.Now, or the system clock.
func (a Agent) clock() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// KIFFToolPrefix starts the name of every tool KIFF's gateway serves itself
// (kiff_card, kiff_pending); any other gateway tool is a vendor's.
const KIFFToolPrefix = "kiff_"

// SteerNote opens a message the owner wrote while the run went, as the model
// reads it: it comes between the agent's turns, not as a new request.
const SteerNote = "The owner wrote this while you were working:\n\n"

// PauseNote follows the results of a turn whose call KIFF holds, in the turn
// the agent gets to say what it waits for: without it, the tool results alone
// end the conversation, and with tools forbidden a model may answer with no
// text, leaving the run with no report (#78).
const PauseNote = "KIFF holds a call above for the owner, so your run pauses here until they answer, and you can't call tools now. " +
	"Write your report: what you did, what waits for the owner, and what you didn't do and why, naming each item."

// CardPauseNote is PauseNote for a run that waits for the owner to answer a
// card it showed (and for KIFF, if it also holds a call).
const CardPauseNote = "Your run pauses here until the owner answers the card above, and you can't call tools now. " +
	"Their answer comes back as the card's result. Say in one short line what you're waiting for; the card itself asks."

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
// An owner's message names the files sent with it, never their bytes.
type TextBody struct {
	Text        string       `json:"text"`
	Attachments []attach.Ref `json:"attachments,omitempty"`
}

// ToolCallBody is the body of a ToolCall event. The input is recorded as the
// model wrote it; secrets never reach it (keys live in the tool adapters).
type ToolCallBody struct {
	CallID string `json:"call_id"`
	Tool   string `json:"tool"`
	// Server is GatewayServer for a call through KIFF's gateway, and empty
	// for one of the agent's own tools: what evals grade where it went.
	Server string          `json:"server,omitempty"`
	Input  json.RawMessage `json:"input"`
}

// GatewayServer is the server a call through KIFF's gateway is recorded
// under: the name Managed Agents gives KIFF's MCP server, so runs of both
// runtimes grade alike.
const GatewayServer = "kiff"

// ToolResultBody is the body of a ToolResult event.
type ToolResultBody struct {
	CallID string `json:"call_id"`
	Tool   string `json:"tool"`
	tool.Result
	// NotMade: the call was refused back to the model unmade, an act sent
	// beside a read (OneAtATime). Nothing reached KIFF or the vendor, so
	// the app shows it as not made, not as a failed call.
	NotMade bool `json:"not_made,omitempty"`
	// took is how long the call took, and batch how many calls ran beside
	// it in the same turn, itself included: recorded on the next turn's
	// Usage (session.ToolTiming), not with the result, which a run drops.
	took  time.Duration
	batch int
}

// TrialWriteBody is the body of a TrialWrite event: a trial's call through
// KIFF's gateway, as made, and KIFF's decision on it (kiff/tafs#107).
type TrialWriteBody struct {
	CallID string          `json:"call_id"`
	Tool   string          `json:"tool"`
	Input  json.RawMessage `json:"input"`
	tool.TrialDecision
}

// CallRefBody is the body of a CallRef event: KIFF's key for a gateway call
// and what it acted on, the only part of its input kept past its run.
type CallRefBody struct {
	Invocation string `json:"invocation"`
	Value      string `json:"value"`
}

// recordRef records what a gateway call acted on, when KIFF named both its
// key and the argument its Card scopes on.
func recordRef(rec *session.Recording, r tool.Result) error {
	if r.Invocation == "" || r.Resource == "" {
		return nil
	}
	return rec.Record(session.CallRef, CallRefBody{Invocation: r.Invocation, Value: r.Resource})
}

// ErrorBody is the body of an Error event. It says what failed, never why:
// events are shown to the tenant, and a cause can carry a backend's own
// diagnostics. The cause goes to the caller, as the error returned.
type ErrorBody struct {
	Message string `json:"message"`
}

// EventKind is what a streamed Event says.
type EventKind string

const (
	TextDelta  EventKind = "text"          // more of the agent's answer
	CallStart  EventKind = "tool_call"     // the agent calls a tool
	CallResult EventKind = "tool_result"   // a tool answered
	Shown      EventKind = "component"     // the agent showed the owner a component
	Steered    EventKind = "owner_message" // the run took a message the owner wrote while it went
	// Thought is more of the summary of the model's thinking (Agent.Thinking),
	// for showing while the run goes. It isn't recorded.
	Thought EventKind = "thinking"
)

// Event is streamed to the caller as the run goes.
type Event struct {
	Kind   EventKind       `json:"kind"`
	Text   string          `json:"text,omitempty"`
	CallID string          `json:"call_id,omitempty"`
	Tool   string          `json:"tool,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"`
	Result *tool.Result    `json:"result,omitempty"`
	// NotMade: a CallResult for a call refused back unmade (OneAtATime), so the
	// app's work block shows it as not made while the run goes on.
	NotMade bool `json:"not_made,omitempty"`
	// Component is what Shown shows: the component as the show tool checked it.
	Component json.RawMessage `json:"component,omitempty"`
}

// Stop is why a run stopped.
type Stop string

const (
	Answered  Stop = "answered"  // the agent answered and called nothing more
	Paused    Stop = "paused"    // KIFF holds at least one call for the owner; the run waits for it
	CostCap   Stop = "cost_cap"  // the run reached its cost cap
	TurnCap   Stop = "turn_cap"  // the run reached its turn limit
	TooLong   Stop = "max_token" // the model ran out of room mid-answer
	Cancelled Stop = "cancelled" // the run was asked to stop, and stopped at a checkpoint
	Refused   Stop = "refused"   // a safety classifier declined the model's turn: Outcome.Refusal says why
)

// Outcome is how a run stopped. Cost and Turns count the whole run, across
// its pauses.
type Outcome struct {
	Stop Stop `json:"stop"`
	// Text is the agent's last answer; for a paused run, what it waits for.
	Text string `json:"text"`
	// Refusal is the category a Refused run's turn was declined for, or ""
	// when the provider named none.
	Refusal string `json:"refusal,omitempty"`
	// Cost is what the run's model turns cost, in millionths of a US dollar.
	Cost int64 `json:"cost_micro_usd"`
	// Turns counts the run's model turns.
	Turns int `json:"turns"`
	// Requirements are what a paused run waits for: one per call KIFF holds,
	// in the order of the calls. Each names the call; KIFF's operation ID
	// and the hold's expiry are left for the caller to fill in from KIFF.
	Requirements []run.Requirement `json:"requirements,omitempty"`
	// Turn is the turn a paused run paused in, to continue from: the model's
	// message, and the results of the calls KIFF didn't hold.
	Turn *run.Turn `json:"turn,omitempty"`
	// Done are the calls that acted and went through in this part of the
	// run (#249), in order: the caller adds them to the run's.
	Done []run.Done `json:"done,omitempty"`

	// standard: a fast agent's turn ran at standard speed, so the rest of
	// the run does too.
	standard bool
}

// ModelFailed is the Error event a failed model turn records.
const ModelFailed = "the model call failed"

// ErrResults means Continue was given results that don't match what the run
// waits for: one result for each requirement, and nothing else.
var ErrResults = errors.New("agent: the results don't match the run's requirements")

// Run runs the agent on input, after history, recording into rec. It returns
// how the run stopped and the conversation with this run's turns added, for a
// chat to continue. It doesn't end or pause rec, or store the run: the
// caller does, as the outcome says.
//
// A run pauses when KIFF holds any of a turn's calls: the calls in that turn
// all run, and the run stops after it, whatever its caps, so a held call
// always has a run to continue. Before it stops, the agent gets one more
// turn, if its caps allow, to say what it waits for: the owner reads that.
// The turn offers the agent's tools but forbids calling them
// (model.Request.NoToolUse), so the agent can't retry around the hold. That
// turn isn't part of what a continued run sees.
func Run(ctx context.Context, a Agent, rec *session.Recording, history []model.Message, input string, emit func(Event)) (Outcome, []model.Message, error) {
	if err := a.Check(); err != nil {
		return Outcome{}, history, err
	}
	asked := append(a.asked(input), attach.Blocks(a.Attachments)...)
	msgs := append(append([]model.Message(nil), history...), model.Message{Role: model.User, Blocks: asked})
	a.history = len(history)
	if err := rec.Record(session.OwnerMessage, TextBody{Text: input, Attachments: a.Attachments}); err != nil {
		return Outcome{}, msgs, err
	}
	if err := a.load(ctx, msgs); err != nil {
		return Outcome{}, msgs, err
	}
	return a.loop(ctx, rec, msgs, Outcome{Cost: a.Spent}, emit)
}

// asked is what the run's input message opens with: the preface, then the
// input, if there is any (a message may be files alone).
func (a Agent) asked(input string) []model.Block {
	asked := slices.Clone(a.Preface)
	if input != "" {
		asked = append(asked, model.Block{Kind: model.Text, Text: Unforged(input)})
	}
	return asked
}

// load fills msgs' files with their bytes, when the agent loads any.
func (a Agent) load(ctx context.Context, msgs []model.Message) error {
	if a.Load == nil {
		return nil
	}
	return a.Load(ctx, msgs)
}

// Continue carries on a paused run, r, as stored, from the turn it paused
// in, with held: the outcome of each call it waits for, as KIFF reports it,
// one per requirement and none still held (ErrResults otherwise). rec is the run's recording,
// resumed; Continue records the held calls' results in it. It reads nothing
// from the run's events, so what the keep rules dropped doesn't matter.
//
// The continued run sees history, its input, its own turns before the one it
// paused in (Turn.Before), and that turn. Its cost and turns carry on from
// r's, so the caps hold across pauses.
func Continue(ctx context.Context, a Agent, rec *session.Recording, history []model.Message, r run.Run, held []ToolResultBody, emit func(Event)) (Outcome, []model.Message, error) {
	if err := a.Check(); err != nil {
		return Outcome{}, history, err
	}
	if r.Paused == nil || len(r.Requirements) == 0 {
		return Outcome{}, history, fmt.Errorf("%w: run %s has no paused turn", ErrResults, r.ID)
	}
	results, err := merge(r, held)
	if err != nil {
		return Outcome{}, history, err
	}
	if emit == nil {
		emit = func(Event) {}
	}
	var done []run.Done
	for _, h := range held {
		if err := rec.Record(session.ToolResult, h); err != nil {
			return Outcome{}, history, err
		}
		if wentThrough(h) {
			done = append(done, doneOf(h, requirementInput(r, h.CallID)))
		}
		if err := recordRef(rec, h.Result); err != nil {
			return Outcome{}, history, err
		}
		if q, ok := requirement(r, h.CallID); ok && q.Card() {
			if err := recordAnswered(rec, q); err != nil {
				return Outcome{}, history, err
			}
		} else if err := recordCalled(rec, nil, h.Tool, requirementInput(r, h.CallID), outcome(h, requirementAnswer(r, h.CallID))); err != nil {
			return Outcome{}, history, err
		}
		emit(Event{Kind: CallResult, CallID: h.CallID, Tool: h.Tool, Result: &h.Result})
	}
	// What the owner wrote while the run went, before it paused, follows its
	// input: the continued run doesn't see the turns between them.
	asked := append(a.asked(r.Input), attach.Blocks(r.Attachments)...)
	for _, text := range r.Taken() {
		asked = append(asked, model.Block{Kind: model.Text, Text: SteerNote + Unforged(text)})
	}
	msgs := append(append([]model.Message(nil), history...), model.Message{Role: model.User, Blocks: asked})
	msgs = append(msgs, r.Paused.Before...)
	// What the run did, from its record, after the results: a continued run
	// says what was done from it, not from the chat (the R-3005 test).
	if note := a.recordNote(r, held); note != "" {
		results = append(results, model.Block{Kind: model.Text, Text: note})
	}
	msgs = append(msgs, withoutThinking(r.Paused.Reply), model.Message{Role: model.User, Blocks: results})
	a.history = len(history)
	if err := a.load(ctx, msgs); err != nil {
		return Outcome{}, msgs, err
	}
	a.resumed = true
	return a.loop(ctx, rec, msgs, Outcome{Cost: r.Cost + a.Spent, Turns: r.Turns, Done: done}, emit)
}

// withoutThinking is a paused turn's reply as a continued run sends it: its
// thinking left out. A thinking block is valid only in the conversation it
// was made in, and a continued run's is another one: its tools are listed
// again, and what the owner wrote meanwhile follows its input. The model
// provider refuses such a block (a 400, so the run would end in ERROR), and
// asks for it to be removed.
func withoutThinking(m model.Message) model.Message {
	blocks := make([]model.Block, 0, len(m.Blocks))
	for _, b := range m.Blocks {
		if b.Kind != model.Thinking && b.Kind != model.RedactedThinking {
			blocks = append(blocks, b)
		}
	}
	m.Blocks = blocks
	return m
}

// merge is the paused turn's results with held's, in the order of the
// calls, if held answers exactly the run's requirements.
func merge(r run.Run, held []ToolResultBody) ([]model.Block, error) {
	want := map[string]string{} // the tool of each call the run waits for
	for _, q := range r.Requirements {
		want[q.CallID] = q.Tool
	}
	byCall := map[string]model.Block{}
	for _, b := range r.Paused.Results {
		byCall[b.CallID] = b
	}
	for _, h := range held {
		if h.Held() {
			return nil, fmt.Errorf("%w: KIFF still holds %q", ErrResults, h.CallID)
		}
		tool, ok := want[h.CallID]
		if !ok {
			return nil, fmt.Errorf("%w: %q isn't a call the run waits for", ErrResults, h.CallID)
		}
		if h.Tool != tool {
			return nil, fmt.Errorf("%w: %q called %s, not %s", ErrResults, h.CallID, tool, h.Tool)
		}
		delete(want, h.CallID)
		byCall[h.CallID] = resultBlock(h)
	}
	for id := range want {
		return nil, fmt.Errorf("%w: no result for %q", ErrResults, id)
	}
	var out []model.Block
	for _, b := range r.Paused.Reply.Blocks {
		if b.Kind != model.ToolUse {
			continue
		}
		res, ok := byCall[b.CallID]
		if !ok {
			return nil, fmt.Errorf("%w: run %s: call %q has no result", ErrResults, r.ID, b.CallID)
		}
		out = append(out, res)
	}
	return out, nil
}

// wentThrough reports whether a call's result says it was made: not refused
// back unmade, not held, not refused by KIFF, and not failed.
func wentThrough(r ToolResultBody) bool {
	return !r.NotMade && !r.IsError && r.State != tool.StateHeld && r.State != tool.StateRefused && r.Trial == nil
}

// doneOf is r, a call that acted and went through, with its input, as the run
// keeps it.
func doneOf(r ToolResultBody, input json.RawMessage) run.Done {
	return run.Done{CallID: r.CallID, Tool: r.Tool, Input: input, Resource: r.Resource, Invocation: r.Invocation}
}

// requirement is the requirement of r's call id.
func requirement(r run.Run, id string) (run.Requirement, bool) {
	for _, q := range r.Requirements {
		if q.CallID == id {
			return q, true
		}
	}
	return run.Requirement{}, false
}

// requirementAnswer is the owner's answer to the call r waits for with id,
// as KIFF reported it.
func requirementAnswer(r run.Run, id string) string {
	for _, q := range r.Requirements {
		if q.CallID == id {
			return q.Answer
		}
	}
	return ""
}

// requirementInput is the input of the call r waits for with id.
func requirementInput(r run.Run, id string) json.RawMessage {
	for _, q := range r.Requirements {
		if q.CallID == id {
			return q.Input
		}
	}
	return nil
}

func resultBlock(r ToolResultBody) model.Block {
	// A vendor's answer can carry anyone's text: never a line of the record.
	return model.Block{Kind: model.ToolResult, CallID: r.CallID, Result: Unforged(r.Text), IsError: r.IsError}
}

// loop runs model turns on msgs until the run stops, counting on from out.
func (a Agent) loop(ctx context.Context, rec *session.Recording, msgs []model.Message, out Outcome, emit func(Event)) (Outcome, []model.Message, error) {
	return a.loopFrom(ctx, rec, msgs, out, emit, false)
}

// loopFrom is loop; checked says its first checkpoint was done already
// (More), so it starts at the model's turn.
func (a Agent) loopFrom(ctx context.Context, rec *session.Recording, msgs []model.Message, out Outcome, emit func(Event), checked bool) (Outcome, []model.Message, error) {
	if emit == nil {
		emit = func(Event) {}
	}
	byName := make(map[string]tool.Tool, len(a.Tools))
	specs := make([]model.ToolSpec, 0, len(a.Tools))
	for _, t := range a.Tools {
		s := t.Spec()
		byName[s.Name] = t
		s.Defer = slices.ContainsFunc(a.Deferred, func(p string) bool { return strings.HasPrefix(s.Name, p) }) ||
			(a.DeferVendor && t.Gateway() && !strings.HasPrefix(s.Name, KIFFToolPrefix))
		specs = append(specs, s)
	}
	var waited []session.ToolTiming // the last turn's tool calls, for the next turn's Usage
	for first := true; ; first = false {
		if !(first && checked) {
			if stop := a.capped(out); stop != "" {
				out.Stop = stop
				return out, msgs, nil
			}
			if a.Cancel != nil {
				stop, err := a.Cancel(ctx)
				if err != nil {
					return out, msgs, err
				}
				if stop {
					out.Stop = Cancelled
					return out, msgs, nil
				}
			}
			if _, err := a.steer(ctx, rec, &msgs, emit); err != nil {
				return out, msgs, err
			}
		}
		reply, err := a.turn(ctx, rec, msgs, specs, false, waited, &out, emit)
		a.resumed = false
		if err != nil {
			return out, msgs, err
		}
		if reply.Stop == model.Refused { // before anything of the turn is kept or run
			out.Stop, out.Refusal = Refused, reply.Refusal
			return out, msgs, nil
		}
		msgs = append(msgs, model.Message{Role: model.Assistant, Blocks: reply.Blocks})
		var calls []model.Block
		for _, b := range reply.Blocks {
			if b.Kind == model.ToolUse {
				calls = append(calls, b)
			}
		}
		if err := a.say(rec, out.Text, len(calls) == 0); err != nil {
			return out, msgs, err
		}
		if len(calls) == 0 {
			out.Stop = Answered
			if reply.Stop == model.MaxTokens {
				out.Stop = TooLong
			}
			return out, msgs, nil
		}

		results, err := a.callTools(ctx, rec, byName, calls, emit)
		if err != nil {
			return out, msgs, err
		}
		blocks := make([]model.Block, len(results))
		var held bool
		waited = make([]session.ToolTiming, len(results))
		for i, r := range results {
			blocks[i] = resultBlock(r)
			held = held || r.Waits()
			if acts(byName[r.Tool], calls[i].Input) && wentThrough(r) {
				out.Done = append(out.Done, doneOf(r, calls[i].Input))
			}
			waited[i] = session.ToolTiming{Tool: r.Tool, MS: r.took.Milliseconds(), ResultBytes: len(r.Text), Batch: r.batch, IsError: r.IsError, NotMade: r.NotMade}
		}
		if held {
			return a.pause(ctx, rec, msgs, out, run.Turn{Reply: msgs[len(msgs)-1]}, results, specs, emit)
		}
		msgs = append(msgs, model.Message{Role: model.User, Blocks: blocks})
		msgs = clearRead(msgs, ClearAfter)
	}
}

// ClearAfter is how many bytes of tool results the model has already read
// a run carries before they're cleared, all at once: a cleared result's
// place changes what the model was sent, so each clearing breaks the
// prompt cache once, never on every turn.
const ClearAfter = 48 << 10

// clearRead is msgs with every tool result the model has already read (all
// but the last message's) replaced by a one-line note, once those pass
// after bytes. The last message's results are new: the next turn reads
// them whole.
func clearRead(msgs []model.Message, after int) []model.Message {
	if len(msgs) < 2 {
		return msgs
	}
	read := msgs[:len(msgs)-1]
	size := 0
	for _, m := range read {
		for _, b := range m.Blocks {
			if b.Kind == model.ToolResult && !cleared(b.Result) {
				size += len(b.Result)
			}
		}
	}
	if size <= after {
		return msgs
	}
	tools := map[string]string{} // call id → tool
	for _, m := range read {
		for _, b := range m.Blocks {
			if b.Kind == model.ToolUse {
				tools[b.CallID] = b.Tool
			}
		}
	}
	out := slices.Clone(msgs)
	for i := range out[:len(out)-1] {
		blocks := slices.Clone(out[i].Blocks)
		for j, b := range blocks {
			if b.Kind == model.ToolResult && !cleared(b.Result) {
				blocks[j].Result = fmt.Sprintf(ClearedNote, tools[b.CallID], (len(b.Result)+1023)>>10)
			}
		}
		out[i].Blocks = blocks
	}
	return out
}

// ClearedNote stands for a tool result the model has read, once cleared:
// the tool and its size in KB.
const ClearedNote = "[cleared: the %s result (%d KB) you read earlier in this task. What you need from it is in what you wrote since; call the tool again if you need more.]"

// cleared reports whether a result is already the note.
func cleared(result string) bool { return strings.HasPrefix(result, "[cleared: ") }

// More carries on a run the agent has answered, out, with what the owner wrote
// while it answered: the messages Steer takes now, each given a turn, as at a
// checkpoint. msgs is the conversation Run or Continue returned. With nothing
// taken, or no turn left under the caps, it returns out as it was: the caller
// asks it last, right before it stores the run as answered.
func More(ctx context.Context, a Agent, rec *session.Recording, msgs []model.Message, out Outcome, emit func(Event)) (Outcome, []model.Message, error) {
	if out.Stop != Answered || a.capped(out) != "" {
		return out, msgs, nil
	}
	if emit == nil {
		emit = func(Event) {}
	}
	took, err := a.steer(ctx, rec, &msgs, emit)
	if err != nil || !took {
		return out, msgs, err
	}
	// This was the checkpoint (the caps above, the caller's cancel check, the
	// steer): the loop starts at the turn, so what was taken is read first.
	return a.loopFrom(ctx, rec, msgs, out, emit, true)
}

// steer takes what the owner wrote while the run went: each message recorded
// as an owner message, streamed, and added after what the model read last,
// in the same user turn when that ends the conversation (a tool batch's
// results, or the run's input). It reports whether it took any.
func (a Agent) steer(ctx context.Context, rec *session.Recording, msgs *[]model.Message, emit func(Event)) (bool, error) {
	if a.Steer == nil {
		return false, nil
	}
	texts, err := a.Steer(ctx)
	if err != nil || len(texts) == 0 {
		return false, err
	}
	var blocks []model.Block
	for _, text := range texts {
		if err := rec.Record(session.OwnerMessage, TextBody{Text: text}); err != nil {
			return false, err
		}
		emit(Event{Kind: Steered, Text: text})
		blocks = append(blocks, model.Block{Kind: model.Text, Text: SteerNote + Unforged(text)})
	}
	m := *msgs
	if n := len(m); n > 0 && m[n-1].Role == model.User {
		// Its own copy of the blocks: the caller's history may share them.
		m[n-1].Blocks = append(slices.Clone(m[n-1].Blocks), blocks...)
	} else {
		m = append(m, model.Message{Role: model.User, Blocks: blocks})
	}
	*msgs = m
	return true, nil
}

// pause stops the run in turn, whose calls returned results: a requirement
// for each held one, the others' results kept with the turn. If the caps
// allow, the agent first gets one turn to say what it waits for, offered its
// tools but forbidden to call them.
func (a Agent) pause(ctx context.Context, rec *session.Recording, msgs []model.Message, out Outcome, turn run.Turn, results []ToolResultBody, specs []model.ToolSpec, emit func(Event)) (Outcome, []model.Message, error) {
	turn.Results = nil
	turn.Before = before(msgs, a.history)
	blocks := make([]model.Block, len(results))
	cards := false
	for i, r := range results {
		blocks[i] = resultBlock(r)
		if !r.Waits() {
			turn.Results = append(turn.Results, blocks[i])
			continue
		}
		if r.State == tool.StateAwaiting {
			// A card: the owner answers it through the API, and it stops
			// waiting after CardWait.
			for _, b := range turn.Reply.Blocks {
				if b.Kind == model.ToolUse && b.CallID == r.CallID {
					out.Requirements = append(out.Requirements, run.Requirement{CallID: r.CallID, Tool: r.Tool, Input: b.Input,
						Kind: run.KindCard, Expires: a.clock().Add(run.CardWait).UTC()})
				}
			}
			cards = true
			continue
		}
		var input json.RawMessage
		for _, b := range turn.Reply.Blocks {
			if b.Kind == model.ToolUse && b.CallID == r.CallID {
				input = b.Input
			}
		}
		q := run.Requirement{CallID: r.CallID, Tool: r.Tool, Input: input}
		if r.Hold != nil {
			q.Operation, q.Exception, q.Expires = r.Hold.Operation, r.Hold.Exception, r.Hold.Expires.UTC()
		}
		out.Requirements = append(out.Requirements, q)
	}
	out.Stop, out.Turn = Paused, &turn
	msgs = append(msgs, model.Message{Role: model.User, Blocks: blocks})
	if a.capped(out) != "" {
		return out, msgs, nil
	}
	// The turn that says what it waits for is told so; the results the run
	// keeps, and Continue sends, stay as they are.
	asked := slices.Clone(msgs)
	note := PauseNote
	if cards {
		note = CardPauseNote
	}
	asked[len(asked)-1].Blocks = append(slices.Clone(blocks), model.Block{Kind: model.Text, Text: note})
	reply, err := a.turn(ctx, rec, asked, specs, true, nil, &out, emit)
	if err != nil {
		return out, msgs, err
	}
	msgs = append(msgs, model.Message{Role: model.Assistant, Blocks: reply.Blocks})
	return out, msgs, a.say(rec, out.Text, true)
}

// BeforeResultMax is the most of one earlier result a paused run keeps for
// when it continues (Turn.Before), in bytes, BeforeMax the most of its
// earlier turns, and BeforeBytes the most they take as JSON, so a paused
// turn stays well under what a store keeps (3 MiB in DynamoDB): long
// assistant turns or tool inputs never stop a run from pausing.
const (
	BeforeResultMax = 8 << 10
	BeforeMax       = 24
	BeforeBytes     = 512 << 10
)

// before is a run's own turns before the one it paused in, as it keeps them
// to continue: msgs is the run's conversation up to and including the paused
// turn's reply, and the run's input is at index start. A turn too long ago
// is left out from the oldest, a whole assistant turn and its results at a
// time, as are turns past the byte budget, and a long result keeps its start.
func before(msgs []model.Message, start int) []model.Message {
	if start+1 >= len(msgs)-1 {
		return nil
	}
	turns := msgs[start+1 : len(msgs)-1]
	for len(turns) > BeforeMax {
		turns = turns[2:] // an assistant turn and the results that answered it
	}
	out := make([]model.Message, 0, len(turns))
	for _, m := range turns {
		m = withoutThinking(m)
		blocks := slices.Clone(m.Blocks)
		for i, b := range blocks {
			if b.Kind == model.ToolResult && len(b.Result) > BeforeResultMax {
				blocks[i].Result = strings.ToValidUTF8(b.Result[:BeforeResultMax], "") + "\n[cut: the rest was here when you read it]"
			}
		}
		m.Blocks = blocks
		out = append(out, m)
	}
	// Within the byte budget too, from the oldest, a pair at a time.
	for len(out) > 0 && jsonSize(out) > BeforeBytes {
		out = out[min(2, len(out)):]
	}
	return out
}

// jsonSize is how many bytes msgs take as JSON, as a store keeps them.
func jsonSize(msgs []model.Message) int {
	raw, err := json.Marshal(msgs)
	if err != nil {
		return 0
	}
	return len(raw)
}

// capped is the cap a run has reached, if any.
func (a Agent) capped(out Outcome) Stop {
	maxTurns := a.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}
	switch {
	case out.Cost >= a.MaxCost:
		return CostCap
	case out.Turns >= maxTurns:
		return TurnCap
	}
	return ""
}

// turn is one model turn, offered tools (to call, unless noToolUse),
// recorded and costed into out, whose Text becomes the turn's text.
func (a Agent) turn(ctx context.Context, rec *session.Recording, msgs []model.Message, tools []model.ToolSpec, noToolUse bool, waited []session.ToolTiming, out *Outcome, emit func(Event)) (model.Reply, error) {
	req := model.Request{Model: a.ModelID, System: a.System, Messages: msgs, MaxTokens: a.MaxTokens, Tools: tools, NoToolUse: noToolUse, Effort: a.Effort, Cache: a.Price.Caches(), CacheLong: a.Price.CachesAnHour(), HistoryEnd: a.history,
		Fast: a.Fast && !out.standard, Thinking: a.Thinking, OnThinking: func(s string) { emit(Event{Kind: Thought, Text: s}) }}
	began := time.Now()
	reply, err := a.Model.Turn(ctx, req, func(s string) { emit(Event{Kind: TextDelta, Text: s}) })
	took := time.Since(began)
	if err != nil {
		_ = rec.Record(session.Error, ErrorBody{Message: ModelFailed})
		return reply, err
	}
	price, speed := a.Price, ""
	switch {
	case reply.Fast:
		price, speed = a.PriceFast, session.SpeedFast
	case a.Fast:
		speed, out.standard = session.SpeedStandard, true
	}
	cost := price.Cost(reply.Usage)
	out.Cost += cost
	out.Turns++
	if err := rec.Record(session.Usage, session.UsageBody{Model: a.ModelID, InputTokens: reply.Usage.InputTokens,
		OutputTokens: reply.Usage.OutputTokens, CacheRead: reply.Usage.CacheRead, CacheWrite: reply.Usage.CacheWrite, CacheWrite1h: reply.Usage.CacheWrite1h, CostMicroUSD: cost,
		Context: a.context(ctx, req), Timing: a.turnTiming(reply, took, out.Turns == 1, waited), Speed: speed}); err != nil {
		return reply, err
	}
	if reply.Stop == model.Refused {
		// A refusal can come after part of the turn streamed: that part is
		// no text of the turn, and the caller keeps and runs none of it.
		// Its usage counts.
		return model.Reply{Stop: reply.Stop, Refusal: reply.Refusal, Usage: reply.Usage}, nil
	}
	var text []string
	for _, b := range reply.Blocks {
		if b.Kind == model.Text {
			text = append(text, b.Text)
		}
	}
	out.Text = strings.TrimSpace(strings.Join(text, "\n"))
	return reply, nil
}

// turnTiming is one model turn's timing: how long it took and streamed
// before its first content, and the sizes of what it wrote, never the
// words, with the tool calls whose results it read; on the run's first
// turn, how long the run took to get to it; on the first after Continue,
// that it was.
func (a Agent) turnTiming(reply model.Reply, took time.Duration, first bool, waited []session.ToolTiming) *session.TurnTiming {
	t := &session.TurnTiming{MS: took.Milliseconds(), FirstMS: reply.FirstToken.Milliseconds(), ThinkingBytes: reply.ThinkingBytes, Waited: waited, Resumed: a.resumed}
	if first {
		t.StartMS = a.Started.Milliseconds()
	}
	for _, b := range reply.Blocks {
		switch b.Kind {
		case model.Text:
			t.TextBytes += len(b.Text)
		case model.ToolUse:
			t.ToolInputBytes += len(b.Input)
			t.ToolCalls++
		}
	}
	return t
}

// context is what req's input was made of. The window is the model's, when
// it knows it; a failure to ask leaves it unknown and fails nothing.
func (a Agent) context(ctx context.Context, req model.Request) *session.ContextBody {
	s := req.Size()
	c := &session.ContextBody{SystemBytes: s.System, ToolsBytes: s.Tools, MessagesBytes: s.Messages,
		ToolResultsBytes: s.ToolResults, Messages: len(req.Messages)}
	if w, ok := a.Model.(model.Windower); ok {
		c.WindowTokens, _ = w.Window(ctx, a.ModelID)
	}
	return c
}

// say records the turn's text: a run's last answer is its Report.
func (a Agent) say(rec *session.Recording, text string, last bool) error {
	if text == "" {
		return nil
	}
	kind := session.AgentMessage
	if last && rec.Session().Kind == session.KindRun {
		kind = session.Report
	}
	return rec.Record(kind, TextBody{Text: text})
}

// callTools records the calls in order, runs them in parallel, and records
// their results in the order of the calls.
func (a Agent) callTools(ctx context.Context, rec *session.Recording, byName map[string]tool.Tool, calls []model.Block, emit func(Event)) ([]ToolResultBody, error) {
	for _, c := range calls {
		server := ""
		if t, ok := byName[c.Tool]; ok && t.Gateway() {
			server = GatewayServer
		}
		if err := rec.Record(session.ToolCall, ToolCallBody{CallID: c.CallID, Tool: c.Tool, Server: server, Input: c.Input}); err != nil {
			return nil, err
		}
		emit(Event{Kind: CallStart, CallID: c.CallID, Tool: c.Tool, Input: c.Input})
	}
	results := make([]ToolResultBody, len(calls))
	// An act goes in no turn with a read: what it acts on was read in an
	// earlier turn, not beside it. Acts side by side go together, and the
	// agent's own calls (its notes, the chat's components) don't count.
	reads := false
	for _, c := range calls {
		t := byName[c.Tool]
		if t == nil || (t.Access() != tool.Own && !acts(t, c.Input)) {
			reads = true
		}
	}
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			began := time.Now()
			if reads && acts(byName[c.Tool], c.Input) {
				results[i] = ToolResultBody{CallID: c.CallID, Tool: c.Tool, Result: tool.Result{Text: OneAtATime, IsError: true}, NotMade: true}
				results[i].batch = len(calls)
				return
			}
			results[i] = ToolResultBody{CallID: c.CallID, Tool: c.Tool, Result: call(ctx, byName[c.Tool], c)}
			results[i].took, results[i].batch = time.Since(began), len(calls)
		}()
	}
	wg.Wait()
	for i, r := range results {
		if err := rec.Record(session.ToolResult, r); err != nil {
			return nil, err
		}
		if err := recordRef(rec, r.Result); err != nil {
			return nil, err
		}
		if r.Trial == nil {
			if err := recordCalled(rec, byName[r.Tool], r.Tool, calls[i].Input, outcome(r, "")); err != nil {
				return nil, err
			}
		}
		if r.Trial != nil {
			if err := rec.Record(session.TrialWrite, TrialWriteBody{CallID: r.CallID, Tool: r.Tool, Input: calls[i].Input, TrialDecision: *r.Trial}); err != nil {
				return nil, err
			}
		}
		emit(Event{Kind: CallResult, CallID: r.CallID, Tool: r.Tool, Result: &r.Result, NotMade: r.NotMade})
		if len(r.Component) > 0 && !r.IsError && shows(byName[r.Tool]) {
			if err := rec.Record(session.Component, r.Component); err != nil {
				return nil, err
			}
			emit(Event{Kind: Shown, CallID: r.CallID, Component: r.Component})
		}
	}
	return results, nil
}

// OneAtATime is what a call that acts gets back, unmade, when the model sent
// it in a turn with a read: a refund asked for beside the look-up meant
// to check it went before the look-up's result was read (kiff/tafs, the 859
// device test). The reads in that turn are made.
const OneAtATime = "Read first: this call acts, so it doesn't go beside a read, and it was not made. Read the other calls' results first, then make it if it's still right."

// acts reports whether a call of t with input changes something outside the
// agent: a Write tool's call that doesn't only read (tool.Reads), the kind
// the Card and Setup use. Its own notes and the chat's components don't count.
func acts(t tool.Tool, input json.RawMessage) bool {
	return t != nil && t.Access() == tool.Write && !tool.Reads(t, input)
}

// shows reports whether t may put a component in the owner's chat.
func shows(t tool.Tool) bool {
	s, ok := t.(tool.Shower)
	return ok && s.Shows()
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
	r.Text = capResult(r.Text, MaxToolResult)
	return r
}

// MaxToolResult bounds what one tool result puts in the model's context, in
// bytes: a ceiling for a runaway export only. A result is read whole, then
// cleared (clearRead), so it isn't carried turn after turn.
const MaxToolResult = 128 << 10

// capResult is text cut to at most n bytes, on a rune, telling the model
// how much it got and to ask again for less.
func capResult(text string, n int) string {
	if len(text) <= n {
		return text
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + fmt.Sprintf("\n…(cut at %d KB of %d KB: this result is too long to read whole. Call again for less, with a filter or a smaller limit, if what you need isn't above.)", n>>10, (len(text)+1023)>>10)
}

// SummarySystem is what the model is told when it summarises the part of a
// conversation that no longer fits its history.
const SummarySystem = `You are an agent in a long conversation with its owner, and its earliest part no longer fits what you read each time. Write a summary of that part for yourself, so you can carry on as if you remembered it.

Keep:
- what the owner asked for, and every decision, limit, amount, currency, mode, name and preference they gave, in their own words;
- what has been done, and what each tool or system answered that still matters (ids, names, statuses);
- what is still open or waiting, and on whom;
- what you told the owner you would do.

Leave out greetings and anything settled that no longer matters. Write short plain lines, at most 400 words, and only the summary.`

// SummaryMaxTokens bounds a summary's reply.
const SummaryMaxTokens = 2048

// Summarize is the model's summary of msgs, the part of a conversation that
// no longer fits, after prior, its summary of what came before them (empty:
// none), recorded with its usage in rec. It returns the summary and its
// cost, in millionths of a US dollar.
func Summarize(ctx context.Context, a Agent, rec *session.Recording, prior string, msgs []model.Message) (string, int64, error) {
	var b strings.Builder
	if prior != "" {
		b.WriteString("Your summary of what came before:\n\n" + prior + "\n\nThen:\n")
	}
	for _, m := range msgs {
		who := "Owner"
		if m.Role == model.Assistant {
			who = "You"
		}
		for _, bl := range m.Blocks {
			if bl.Kind == model.Text && strings.TrimSpace(bl.Text) != "" {
				b.WriteString("\n" + who + ": " + bl.Text + "\n")
			}
		}
	}
	b.WriteString("\nWrite the summary now.")
	req := model.Request{Model: a.ModelID, System: SummarySystem, MaxTokens: SummaryMaxTokens, Effort: a.Effort,
		Messages: []model.Message{{Role: model.User, Blocks: []model.Block{{Kind: model.Text, Text: b.String()}}}}}
	reply, err := a.Model.Turn(ctx, req, func(string) {})
	if err != nil {
		return "", 0, err
	}
	cost := a.Price.Cost(reply.Usage)
	if err := rec.Record(session.Usage, session.UsageBody{Model: a.ModelID, InputTokens: reply.Usage.InputTokens,
		OutputTokens: reply.Usage.OutputTokens, CacheRead: reply.Usage.CacheRead, CacheWrite: reply.Usage.CacheWrite,
		CacheWrite1h: reply.Usage.CacheWrite1h, CostMicroUSD: cost}); err != nil {
		return "", cost, err
	}
	var text []string
	for _, bl := range reply.Blocks {
		if bl.Kind == model.Text {
			text = append(text, bl.Text)
		}
	}
	out := strings.TrimSpace(strings.Join(text, "\n"))
	if reply.Stop == model.Refused || out == "" {
		return "", cost, errors.New("agent: the model wrote no summary")
	}
	return out, cost, nil
}
