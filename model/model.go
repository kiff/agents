// Package model is how an agent talks to a language model, without knowing
// which one. A Model streams one turn: the text as it comes, then the whole
// reply with its tool calls and what it used. Each provider is an adapter
// (model/anthropic is the first).
package model

import (
	"context"
	"encoding/json"
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
	// tools, and keeping them unchanged keeps the cached prompt prefix.
	NoToolUse bool
}

// Stop is why the model ended its turn.
type Stop string

const (
	EndTurn   Stop = "end_turn"   // it answered
	CallTools Stop = "tool_use"   // it waits for its tool calls' results
	MaxTokens Stop = "max_tokens" // it ran out of room mid-answer
	Other     Stop = "other"
)

// Usage is what one turn used, in tokens.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	CacheWrite   int64
}

// Reply is a whole turn: its blocks in order (text and tool calls), why it
// stopped, and what it used.
type Reply struct {
	Blocks []Block
	Stop   Stop
	Usage  Usage
}

// Model runs one turn. onText, when set, receives the reply's text as it
// streams; the Reply holds all of it once the turn is done.
type Model interface {
	Turn(ctx context.Context, req Request, onText func(string)) (Reply, error)
}

// Price is what a model charges, in millionths of a US dollar per million
// tokens of each kind. It's configuration, like the model id.
type Price struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// Cost is what u costs at p, in millionths of a US dollar, rounded up so a
// cap is never undercounted.
func (p Price) Cost(u Usage) int64 {
	total := u.InputTokens*p.Input + u.OutputTokens*p.Output + u.CacheRead*p.CacheRead + u.CacheWrite*p.CacheWrite
	return (total + 999_999) / 1_000_000
}
