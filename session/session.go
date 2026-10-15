// Package session records what an agent's runs and chats do, under rules the
// operator sets: which kinds of event are kept, until when, and which are
// never written. A Store keeps sessions; a Recorder applies the rules before
// anything reaches it.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Kind is what a session is for.
type Kind string

const (
	KindRun  Kind = "run"  // started by a schedule, a trigger or the owner, to do the job
	KindChat Kind = "chat" // the owner talking to the agent
)

// Status is where a session is. A session that has ended takes no more events.
type Status string

const (
	Running Status = "running"
	Ended   Status = "ended"
	Failed  Status = "failed"
)

// Session is one run or chat of one agent.
type Session struct {
	ID      string    `json:"id"`
	Tenant  string    `json:"tenant"`
	Agent   string    `json:"agent"`
	Kind    Kind      `json:"kind"`
	Status  Status    `json:"status"`
	Started time.Time `json:"started"`
	Updated time.Time `json:"updated"`
}

// EventKind is what an event records. Each kind has its own keep rule.
type EventKind string

const (
	OwnerMessage EventKind = "owner_message" // what the owner wrote, or a run's opening instruction
	AgentMessage EventKind = "agent_message" // what the agent said
	ToolCall     EventKind = "tool_call"     // a tool the agent called, with its arguments
	ToolResult   EventKind = "tool_result"   // what the tool returned
	Report       EventKind = "report"        // the agent's report at the end of a run
	MemoryNote   EventKind = "memory_note"   // a note the agent wrote to its memory
	Usage        EventKind = "usage"         // tokens and cost of one model call
	Error        EventKind = "error"         // what went wrong
)

// EventKinds lists every kind, so a Policy can be checked for gaps.
var EventKinds = []EventKind{OwnerMessage, AgentMessage, ToolCall, ToolResult, Report, MemoryNote, Usage, Error}

// Event is one thing that happened in a session. Seq orders a session's events
// and is set by the Store. Expires is when the event may be purged (zero: it
// lives as long as its session); UntilEnd events are dropped when the session
// ends. Events are written by one goroutine per session (see Recording), so a Store
// sees each session's appends one at a time.
type Event struct {
	Session  string          `json:"session"`
	Seq      int64           `json:"seq"`
	Time     time.Time       `json:"time"`
	Kind     EventKind       `json:"kind"`
	Body     json.RawMessage `json:"body"`
	Expires  time.Time       `json:"expires,omitzero"`
	UntilEnd bool            `json:"until_end,omitempty"`
}

// UsageBody is the body of a Usage event: what one model call used and cost.
// Cost is in millionths of a US dollar, so it sums exactly.
type UsageBody struct {
	Model        string `json:"model"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	CacheRead    int64  `json:"cache_read_tokens,omitempty"`
	CacheWrite   int64  `json:"cache_write_tokens,omitempty"`
	CostMicroUSD int64  `json:"cost_micro_usd"`
}

// ErrNotFound means no such session for this tenant; ErrEnded, that it took no
// more events.
var (
	ErrNotFound = errors.New("session: not found")
	ErrEnded    = errors.New("session: ended")
)

// Store keeps sessions and their events. Every read and delete names the
// tenant, so one tenant never reaches another's sessions. A Store doesn't
// apply keep rules; a Recorder does, and sets Expires and UntilEnd.
type Store interface {
	// Create stores a new session.
	Create(ctx context.Context, s Session) error
	// Append adds events to a running session, in order and all or none,
	// numbering them on from the session's last Seq.
	Append(ctx context.Context, tenant, id string, events []Event) error
	// Read returns a session and its events in order.
	Read(ctx context.Context, tenant, id string) (Session, []Event, error)
	// List returns an agent's sessions, most recently updated first, at most limit.
	List(ctx context.Context, tenant, agent string, limit int) ([]Session, error)
	// End sets a running session's final status and drops its UntilEnd events.
	End(ctx context.Context, tenant, id string, status Status) error
	// Delete removes a session and its events.
	Delete(ctx context.Context, tenant, id string) error
	// DeleteAgent removes all of an agent's sessions and events.
	DeleteAgent(ctx context.Context, tenant, agent string) error
	// Purge removes events whose Expires is before now and returns how many.
	Purge(ctx context.Context, now time.Time) (int, error)
}
