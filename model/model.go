// Package model is how an agent talks to a language model, without knowing
// which one. A Model streams one turn: the text as it comes, then the whole
// reply with its tool calls and what it used. Each provider is an adapter
// (model/anthropic is the first).
package model

import (
	"context"
	"encoding/json"
	"time"
)

// Role is who wrote a message.
type Role string

const (
	User      Role = "user"
	Assistant Role = "assistant"
)

// BlockKind is what a block of a message holds.
type BlockKind string

const (
	Text       BlockKind = "text"        // Text
	ToolUse    BlockKind = "tool_use"    // the model calls Tool with Input, as CallID
	ToolResult BlockKind = "tool_result" // what CallID returned: Result, IsError
	// Thinking is the model's reasoning before what follows it in the reply:
	// Text, with Signature. RedactedThinking is reasoning the provider holds
	// encrypted: Data. Both are opaque: they go back to the provider exactly
	// as they came, in place, and nothing else reads them.
	Thinking         BlockKind = "thinking"
	RedactedThinking BlockKind = "redacted_thinking"
	// Server is a step the provider ran itself within the reply, such as
	// finding a deferred tool (ToolSpec.Defer): Data, in the provider's own
	// encoding. Like thinking, it goes back exactly as it came, in place.
	Server BlockKind = "server"
)

// Block is one part of a message.
type Block struct {
	Kind    BlockKind       `json:"kind"`
	Text    string          `json:"text,omitempty"`
	CallID  string          `json:"call_id,omitempty"`
	Tool    string          `json:"tool,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Result  string          `json:"result,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
	// Signature and Data belong to a Thinking or RedactedThinking block;
	// Data to a Server block too.
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
}

// Message is one turn of the conversation.
type Message struct {
	Role   Role    `json:"role"`
	Blocks []Block `json:"blocks"`
}

// ToolSpec is a tool as the model sees it. InputSchema is a JSON Schema object.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
	// Defer keeps the tool's definition out of the prompt: the model finds it
	// by searching when it needs it, so a turn that doesn't pays nothing for
	// it. A provider that can't search sends it like any other.
	Defer bool `json:"defer,omitempty"`
}

// Request is one turn to run. Model is the provider's model id: configuration,
// never a constant in code. No Tools means the model can only answer in text.
type Request struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []ToolSpec
	MaxTokens int64
	// NoToolUse offers Tools but forbids calling any of them this turn: the
	// model must answer in text. Use it, not an empty Tools, when Messages
	// hold tool calls or results: a request with those must still define its
	// tools, and keeping them unchanged keeps the cached prompt prefix. With
	// no Tools it changes nothing.
	NoToolUse bool
	// Effort is how hard the model works on the turn ("low" to "max"), for
	// providers that take it; empty is the provider's default.
	Effort string
	// Fast asks for the provider's fast output, at its higher price, for a
	// turn someone waits on. A provider that can't serve it fast right now
	// (its own, lower limits) runs the turn at standard speed: Reply.Fast
	// says which it was.
	Fast bool
	// Cache asks the provider to cache the prompt: its stable prefix (tools
	// and system prompt) and the conversation up to its last message, so the
	// next turn reads them from the cache. Writing the cache costs more than
	// plain input, so set it only when the turn's Price counts it.
	Cache bool
	// CacheLong keeps the cached shared part (tools and system prompt) an
	// hour instead of five minutes. Set it only when the turn's Price counts
	// an hour's writes (Price.CachesAnHour).
	CacheLong bool
	// HistoryEnd is how many of Messages, from the first, are the run's
	// history: earlier runs as the session keeps them. With Cache, the last of
	// them also takes a breakpoint: the next run renders the same history plus
	// this run's messages, so it reads that prefix back. The breakpoint on the
	// last message can't serve it, since the next run drops this run's tool
	// calls and results.
	HistoryEnd int
}

// Efforts are the effort levels a Request may name, lowest first.
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// Stop is why the model ended its turn.
type Stop string

const (
	EndTurn   Stop = "end_turn"   // it answered
	CallTools Stop = "tool_use"   // it waits for its tool calls' results
	MaxTokens Stop = "max_tokens" // it ran out of room mid-answer
	Refused   Stop = "refusal"    // a safety classifier declined the turn: Reply.Refusal says why
	Other     Stop = "other"
)

// Usage is what one turn used, in tokens.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	CacheWrite   int64
	// CacheWrite1h is what the turn wrote to the cache for an hour, apart
	// from CacheWrite (five minutes): it costs more (Price.CacheWrite1h).
	CacheWrite1h int64
}

// Reply is a whole turn: its blocks in order (text and tool calls), why it
// stopped, and what it used.
type Reply struct {
	Blocks []Block
	Stop   Stop
	Usage  Usage
	// Refusal is the category a Refused turn was declined for ("cyber",
	// "bio", …), or "" when the provider named none.
	Refusal string
	// FirstToken is how long the turn took to stream its first content (a
	// thinking, text or tool-input delta), when the provider streams; 0 when
	// unknown. ThinkingBytes is the size of the thinking it streamed.
	FirstToken    time.Duration
	ThinkingBytes int
	// Fast says the turn ran at the provider's fast speed (Request.Fast), so
	// its Usage costs at the fast price.
	Fast bool
}

// Model runs one turn. onText, when set, receives the reply's text as it
// streams; the Reply holds all of it once the turn is done.
type Model interface {
	Turn(ctx context.Context, req Request, onText func(string)) (Reply, error)
}

// Price is what a model charges, in millionths of a US dollar per million
// tokens of each kind. It's configuration, like the model id. A Price
// without both cache prices Caches nothing: Cost would count cache tokens
// as free.
type Price struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	// CacheWrite1h is what writing to the cache for an hour costs. Set, the
	// part every turn and run of an agent shares (its tools and system
	// prompt) is kept an hour, so an owner who writes again after a pause
	// doesn't wait while it is read again; the conversation stays five
	// minutes. Zero: everything is kept five minutes.
	CacheWrite1h int64
}

// Caches says whether p counts cache tokens, so a turn at p may ask for the
// cache (Request.Cache).
func (p Price) Caches() bool { return p.CacheRead > 0 && p.CacheWrite > 0 }

// CachesAnHour says whether p counts an hour's cache writes too, so a turn at
// p may keep its shared part an hour (Request.CacheLong).
func (p Price) CachesAnHour() bool { return p.Caches() && p.CacheWrite1h > 0 }

// Cost is what u costs at p, in millionths of a US dollar, rounded up so a
// cap is never undercounted.
func (p Price) Cost(u Usage) int64 {
	total := u.InputTokens*p.Input + u.OutputTokens*p.Output + u.CacheRead*p.CacheRead + u.CacheWrite*p.CacheWrite +
		u.CacheWrite1h*p.CacheWrite1h
	return (total + 999_999) / 1_000_000
}

// Size is how big each part of a Request is, in bytes of what the model
// reads: a turn's tokens follow its bytes closely enough to say which part
// takes the room. ToolResults is the part of Messages that tool results take.
type Size struct {
	System      int
	Tools       int
	Messages    int
	ToolResults int
}

// Size measures r.
func (r Request) Size() Size {
	s := Size{System: len(r.System)}
	for _, t := range r.Tools {
		if t.Defer { // out of the prompt until a search finds it
			continue
		}
		s.Tools += len(t.Name) + len(t.Description) + len(t.InputSchema)
	}
	for _, m := range r.Messages {
		for _, b := range m.Blocks {
			n := len(b.Text) + len(b.Tool) + len(b.Input) + len(b.Result) + len(b.Data)
			s.Messages += n
			if b.Kind == ToolResult {
				s.ToolResults += n
			}
		}
	}
	return s
}

// Windower is a Model that knows a model's context window: the most input
// tokens a turn may have.
type Windower interface {
	Window(ctx context.Context, model string) (int64, error)
}
