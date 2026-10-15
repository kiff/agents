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

// Session is one run or chat of one agent. Expires is when the whole session,
// with its events, may be removed (zero: kept until deleted). It comes from
// the keep rules, and moves on to the Expires of each kept event recorded;
// events kept only until the end, which have no Expires, don't move it.
type Session struct {
	ID      string    `json:"id"`
	Tenant  string    `json:"tenant"`
	Agent   string    `json:"agent"`
	Kind    Kind      `json:"kind"`
	Status  Status    `json:"status"`
	Started time.Time `json:"started"`
	Updated time.Time `json:"updated"`
	Expires time.Time `json:"expires,omitzero"`
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

// Event is one thing that happened in a session. Seq numbers a session's
// events from 1, without gaps, and is set by the Recording, the session's only
// writer. Expires is when the event may be purged (zero: it lives as long as
// its session); UntilEnd events are dropped when the session ends. Body
// belongs to the event: a Store copies it in and out.
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

// MaxBody is the largest event body, encoded. A bigger one is recorded as an
// OmittedBody instead, so every Store can keep any event and a large tool
// result never ends a run. Whatever produced it should still be cut down
// before a model reads it: that is what keeps it in the record.
const MaxBody = 256 << 10

// OmittedBody stands in for a body over MaxBody: the event is recorded, with
// its kind and time, and says how big the body was.
type OmittedBody struct {
	Omitted string `json:"omitted"` // why: "over MaxBody"
	Bytes   int    `json:"bytes"`   // the encoded size of the body left out
}

// ErrNotFound means no such session for this tenant; ErrEnded, that it takes no
// more events; ErrGap, that an Append skipped a Seq.
var (
	ErrNotFound = errors.New("session: not found")
	ErrEnded    = errors.New("session: ended")
	ErrGap      = errors.New("session: events out of sequence")
)

// Store keeps sessions and their events. Every read and delete names the
// tenant, so one tenant never reaches another's sessions. A Store doesn't
// apply keep rules; a Recorder does, and sets Expires and UntilEnd.
//
// What has expired is gone as far as callers can tell: from its Expires on,
// Read and List leave it out, by the Store's own clock. Removing it for real is
// Purge's job, or the database's own expiry (DynamoDB's TTL), which costs
// nothing.
type Store interface {
	// Create stores a new session.
	Create(ctx context.Context, s Session) error
	// Append adds events to a running session, all or none. An event whose
	// Seq the session already reached is skipped, so a retried Append stores
	// nothing twice; any other Seq must follow on without a gap (ErrGap). The
	// Store keeps its own copy of each Body. The session's Updated becomes the
	// last event's Time, and its Expires (unless zero) moves on to the latest
	// event Expires.
	Append(ctx context.Context, tenant, id string, events []Event) error
	// Read returns a session and its events in order. The events are the
	// caller's: changing them doesn't change what's stored.
	Read(ctx context.Context, tenant, id string) (Session, []Event, error)
	// List returns an agent's sessions, most recently updated first, at most limit.
	List(ctx context.Context, tenant, agent string, limit int) ([]Session, error)
	// End sets a running session's final status and drops its UntilEnd events.
	End(ctx context.Context, tenant, id string, status Status) error
	// Delete removes a session and its events.
	Delete(ctx context.Context, tenant, id string) error
	// DeleteAgent removes all of an agent's sessions and events.
	DeleteAgent(ctx context.Context, tenant, agent string) error
	// Purge removes events and sessions whose Expires is before now, and
	// returns how many events it removed. A Store whose database expires items
	// by itself may remove nothing here.
	Purge(ctx context.Context, now time.Time) (int, error)
}
