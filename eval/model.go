package eval

import (
	"context"
	"encoding/json"
)

// Model has the shape of model.Model (#2), which the judge uses. Until that
// port is on main, these types stand in for it, field for field, so the judge
// moves over by swapping the import.
type Model interface {
	Turn(ctx context.Context, req Request, onText func(string)) (Reply, error)
}

// Role is who wrote a message.
type Role string

const (
	User      Role = "user"
	Assistant Role = "assistant"
)

// BlockKind is what a block of a message holds.
type BlockKind string

const (
	Text       BlockKind = "text"
	ToolUse    BlockKind = "tool_use"
	ToolResult BlockKind = "tool_result"
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

// ToolSpec is a tool as the model sees it.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Request is one turn to run. Model is the provider's model id.
type Request struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []ToolSpec
	MaxTokens int64
}

// Stop is why the model ended its turn.
type Stop string

const (
	EndTurn   Stop = "end_turn"
	CallTools Stop = "tool_use"
	MaxTokens Stop = "max_tokens"
	Other     Stop = "other"
)

// Usage is what one turn used, in tokens.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	CacheWrite   int64
}

// Reply is a whole turn.
type Reply struct {
	Blocks []Block
	Stop   Stop
	Usage  Usage
}

// Price is what a model charges, in millionths of a US dollar per million
// tokens of each kind.
type Price struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// Cost is what u costs at p, in millionths of a US dollar, rounded up.
func (p Price) Cost(u Usage) int64 {
	total := u.InputTokens*p.Input + u.OutputTokens*p.Output + u.CacheRead*p.CacheRead + u.CacheWrite*p.CacheWrite
	return (total + 999_999) / 1_000_000
}
