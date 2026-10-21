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
	"unicode/utf8"

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

	// System is the agent's instructions, with KIFF's skill in them.
	System string
	Tools  []tool.Tool

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
}

// SteerNote opens a message the owner wrote while the run went, as the model
// reads it: it comes between the agent's turns, not as a new request.
const SteerNote = "The owner wrote this while you were working:\n\n"

// PauseNote follows the results of a turn whose call KIFF holds, in the turn
// the agent gets to say what it waits for: without it, the tool results alone
// end the conversation, and with tools forbidden a model may answer with no
// text, leaving the run with no report (#78).
const PauseNote = "KIFF holds a call above for the owner, so your run pauses here until they answer, and you can't call tools now. " +
	"Write your report: what you did, what waits for the owner, and what you didn't do and why, naming each item."

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
)

// Event is streamed to the caller as the run goes.
type Event struct {
	Kind   EventKind       `json:"kind"`
	Text   string          `json:"text,omitempty"`
	CallID string          `json:"call_id,omitempty"`
	Tool   string          `json:"tool,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"`
	Result *tool.Result    `json:"result,omitempty"`
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
	asked := append(slices.Clone(a.Preface), model.Block{Kind: model.Text, Text: input})
	msgs := append(append([]model.Message(nil), history...), model.Message{Role: model.User, Blocks: asked})
	if err := rec.Record(session.OwnerMessage, TextBody{Text: input}); err != nil {
		return Outcome{}, msgs, err
	}
	return a.loop(ctx, rec, msgs, Outcome{Cost: a.Spent}, emit)
}

// Continue carries on a paused run, r, as stored, from the turn it paused
// in, with held: the outcome of each call it waits for, as KIFF reports it,
// one per requirement and none still held (ErrResults otherwise). rec is the run's recording,
// resumed; Continue records the held calls' results in it. It reads nothing
// from the run's events, so what the keep rules dropped doesn't matter.
//
// The continued run sees history, its input and the turn it paused in, not
// any earlier turns of its own. Its cost and turns carry on from r's, so the
// caps hold across pauses.
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
	for _, h := range held {
		if err := rec.Record(session.ToolResult, h); err != nil {
			return Outcome{}, history, err
		}
		if err := recordRef(rec, h.Result); err != nil {
			return Outcome{}, history, err
		}
		emit(Event{Kind: CallResult, CallID: h.CallID, Tool: h.Tool, Result: &h.Result})
	}
	// What the owner wrote while the run went, before it paused, follows its
	// input: the continued run doesn't see the turns between them.
	asked := append(slices.Clone(a.Preface), model.Block{Kind: model.Text, Text: r.Input})
	for _, text := range r.Taken() {
		asked = append(asked, model.Block{Kind: model.Text, Text: SteerNote + text})
	}
	msgs := append(append([]model.Message(nil), history...),
		model.Message{Role: model.User, Blocks: asked},
		withoutThinking(r.Paused.Reply),
		model.Message{Role: model.User, Blocks: results})
	return a.loop(ctx, rec, msgs, Outcome{Cost: r.Cost + a.Spent, Turns: r.Turns}, emit)
}

// withoutThinking is a paused turn's reply as a continued run sends it: its
// thinking left out. A thinking block is valid only in the conversation it
// was made in, and a continued run's is another one: it leaves out the turns
// between the input and the paused one, and its tools are listed again. The
// model provider refuses such a block (a 400, so the run would end in
// ERROR), and asks for it to be removed.
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

func resultBlock(r ToolResultBody) model.Block {
	return model.Block{Kind: model.ToolResult, CallID: r.CallID, Result: r.Text, IsError: r.IsError}
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
		byName[t.Spec().Name] = t
		specs = append(specs, t.Spec())
	}
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
		reply, err := a.turn(ctx, rec, msgs, specs, false, &out, emit)
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
		for i, r := range results {
			blocks[i] = resultBlock(r)
			held = held || r.Held()
		}
		if held {
			return a.pause(ctx, rec, msgs, out, run.Turn{Reply: msgs[len(msgs)-1]}, results, specs, emit)
		}
		msgs = append(msgs, model.Message{Role: model.User, Blocks: blocks})
	}
}

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
		blocks = append(blocks, model.Block{Kind: model.Text, Text: SteerNote + text})
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
	blocks := make([]model.Block, len(results))
	for i, r := range results {
		blocks[i] = resultBlock(r)
		if !r.Held() {
			turn.Results = append(turn.Results, blocks[i])
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
	asked[len(asked)-1].Blocks = append(slices.Clone(blocks), model.Block{Kind: model.Text, Text: PauseNote})
	reply, err := a.turn(ctx, rec, asked, specs, true, &out, emit)
	if err != nil {
		return out, msgs, err
	}
	msgs = append(msgs, model.Message{Role: model.Assistant, Blocks: reply.Blocks})
	return out, msgs, a.say(rec, out.Text, true)
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
func (a Agent) turn(ctx context.Context, rec *session.Recording, msgs []model.Message, tools []model.ToolSpec, noToolUse bool, out *Outcome, emit func(Event)) (model.Reply, error) {
	req := model.Request{Model: a.ModelID, System: a.System, Messages: msgs, MaxTokens: a.MaxTokens, Tools: tools, NoToolUse: noToolUse, Effort: a.Effort, Cache: a.Price.Caches(), CacheLong: a.Price.CachesAnHour()}
	reply, err := a.Model.Turn(ctx, req, func(s string) { emit(Event{Kind: TextDelta, Text: s}) })
	if err != nil {
		_ = rec.Record(session.Error, ErrorBody{Message: ModelFailed})
		return reply, err
	}
	cost := a.Price.Cost(reply.Usage)
	out.Cost += cost
	out.Turns++
	if err := rec.Record(session.Usage, session.UsageBody{Model: a.ModelID, InputTokens: reply.Usage.InputTokens,
		OutputTokens: reply.Usage.OutputTokens, CacheRead: reply.Usage.CacheRead, CacheWrite: reply.Usage.CacheWrite, CacheWrite1h: reply.Usage.CacheWrite1h, CostMicroUSD: cost,
		Context: a.context(ctx, req)}); err != nil {
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
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = ToolResultBody{CallID: c.CallID, Tool: c.Tool, Result: call(ctx, byName[c.Tool], c)}
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
		if r.Trial != nil {
			if err := rec.Record(session.TrialWrite, TrialWriteBody{CallID: r.CallID, Tool: r.Tool, Input: calls[i].Input, TrialDecision: *r.Trial}); err != nil {
				return nil, err
			}
		}
		if !r.IsError && (r.State == "" || r.State == "forwarded") && r.Trial == nil && tool.Reads(byName[r.Tool], calls[i].Input) {
			if err := rec.Record(session.KeptRead, session.KeptReadBody{CallID: r.CallID, Tool: r.Tool, Input: calls[i].Input, Result: clip(r.Text, MaxKeptRead)}); err != nil {
				return nil, err
			}
		}
		emit(Event{Kind: CallResult, CallID: r.CallID, Tool: r.Tool, Result: &r.Result})
		if len(r.Component) > 0 && !r.IsError && shows(byName[r.Tool]) {
			if err := rec.Record(session.Component, r.Component); err != nil {
				return nil, err
			}
			emit(Event{Kind: Shown, CallID: r.CallID, Component: r.Component})
		}
	}
	return results, nil
}

// MaxKeptRead bounds what one kept read keeps of its result, in bytes: a
// chat's next runs get what it said, not a whole export.
const MaxKeptRead = 16 << 10

// clip is text cut to at most n bytes, on a rune, saying so.
func clip(text string, n int) string {
	if len(text) <= n {
		return text
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + " …(shortened)"
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
	return r
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
