// Package session records what an agent's runs do, under rules the operator
// sets: which kinds of event are kept, until when, and which are never
// written. A session is a thread of runs for one agent; each run records its
// own events. A Store keeps sessions; a Recorder applies the rules before
// anything reaches it.
package session

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

// Kind is what a session is for.
type Kind string

const (
	KindRun  Kind = "run"  // a job: started by a schedule, a trigger or the owner, and its continuations
	KindChat Kind = "chat" // the owner talking to the agent
)

// Session is a thread of runs for one agent. Expires is when the whole
// session, with its events, may be removed (zero: kept until deleted). It
// comes from the keep rules, moves on to the Expires of each kept event
// recorded (events kept only until their run ends, which have no Expires,
// don't move it), and starts again when a run ends.
//
// Held lists the runs that paused in the session and haven't ended. While any
// has, the session doesn't expire, whatever its Expires says.
type Session struct {
	ID      string    `json:"id"`
	Tenant  string    `json:"tenant"`
	Agent   string    `json:"agent"`
	Kind    Kind      `json:"kind"`
	Started time.Time `json:"started"`
	Updated time.Time `json:"updated"`
	Expires time.Time `json:"expires,omitzero"`
	Held    []string  `json:"held,omitempty"`
}

// Expired reports whether the session is gone as of now: its Expires has
// passed and no run in it is held.
func (s Session) Expired(now time.Time) bool {
	return len(s.Held) == 0 && !s.Expires.IsZero() && s.Expires.Before(now)
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

// Event is one thing that happened in a run. Seq numbers a run's events from
// 1, without gaps, and is set by the Recording, the run's only writer. Expires
// is when the event may be purged (zero: it lives as long as its session);
// UntilEnd events are dropped when their run ends. Body belongs to the event:
// a Store copies it in and out.
type Event struct {
	Session  string          `json:"session"`
	Run      string          `json:"run"`
	Seq      int64           `json:"seq"`
	Time     time.Time       `json:"time"`
	Kind     EventKind       `json:"kind"`
	Body     json.RawMessage `json:"body"`
	Expires  time.Time       `json:"expires,omitzero"`
	UntilEnd bool            `json:"until_end,omitempty"`
}

// SortEvents puts a session's events in the order a Store returns them: run by
// run, each in Seq order, the runs in the order of their first event.
func SortEvents(events []Event) {
	type head struct {
		seq  int64
		time time.Time
	}
	first := map[string]head{} // each run's earliest event still stored
	for _, e := range events {
		if h, ok := first[e.Run]; !ok || e.Seq < h.seq {
			first[e.Run] = head{e.Seq, e.Time}
		}
	}
	slices.SortFunc(events, func(a, b Event) int {
		return cmp.Or(first[a.Run].time.Compare(first[b.Run].time), cmp.Compare(a.Run, b.Run), cmp.Compare(a.Seq, b.Seq))
	})
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

// ErrNotFound means no such session for this tenant; ErrEnded, that a run
// takes no more events; ErrGap, that an Append skipped a Seq.
var (
	ErrNotFound = errors.New("session: not found")
	ErrEnded    = errors.New("session: run ended")
	ErrGap      = errors.New("session: events out of sequence")
)

// RunState is what a Store knows of one run's recording: the last Seq it
// stored, whether the run is held (paused), and whether it has ended.
type RunState struct {
	Seq   int64 `json:"seq"`
	Held  bool  `json:"held,omitempty"`
	Ended bool  `json:"ended,omitempty"`
}

// Store keeps sessions and their runs' events. Every call names the tenant,
// so one tenant never reaches another's sessions. A Store doesn't apply keep
// rules; a Recorder does, and sets Expires and UntilEnd.
//
// What has expired is gone as far as callers can tell: from its Expires on,
// Read, ReadRun and List leave it out, by the Store's own clock; a session
// that holds a run never expires. Removing it for real is Purge's job, or the
// database's own expiry (DynamoDB's TTL), which costs nothing.
//
// Runs in one session record independently, and may record at once.
type Store interface {
	// Create stores a new session.
	Create(ctx context.Context, s Session) error
	// Append adds a run's events, all or none. An event whose Seq the run
	// already reached is skipped, so a retried Append stores nothing twice;
	// any other Seq must follow on without a gap (ErrGap). A run that has
	// ended takes no more (ErrEnded). The Store keeps its own copy of each
	// Body. The session's Updated becomes the last event's Time, and its
	// Expires (unless zero) moves on to the latest event Expires.
	Append(ctx context.Context, tenant, session, run string, events []Event) error
	// Read returns a session and its events, in SortEvents order. The events
	// are the caller's: changing them doesn't change what's stored.
	Read(ctx context.Context, tenant, id string) (Session, []Event, error)
	// ReadRun returns what is recorded of one run of a session, and its
	// events in order. A run with nothing recorded has a zero RunState.
	ReadRun(ctx context.Context, tenant, session, run string) (RunState, []Event, error)
	// List returns an agent's sessions, most recently updated first, at most limit.
	List(ctx context.Context, tenant, agent string, limit int) ([]Session, error)
	// Hold marks a run paused: the session doesn't expire until the run ends.
	// Holding a held run again changes nothing; an ended run can't be held
	// (ErrEnded).
	Hold(ctx context.Context, tenant, session, run string) error
	// EndRun ends a run: it takes no more events, its UntilEnd events are
	// dropped, and its hold is released. The session's Expires (unless zero)
	// moves on to expires, if later: retention starts again when a run ends.
	// Ending an ended run is ErrEnded.
	EndRun(ctx context.Context, tenant, session, run string, expires time.Time) error
	// Delete removes a session and its events.
	Delete(ctx context.Context, tenant, id string) error
	// DeleteAgent removes all of an agent's sessions and events.
	DeleteAgent(ctx context.Context, tenant, agent string) error
	// Purge removes events and sessions whose Expires is before now, and
	// returns how many events it removed. A Store whose database expires items
	// by itself may remove nothing here.
	Purge(ctx context.Context, now time.Time) (int, error)
}
