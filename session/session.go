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
	// KindTrial is a trial of a version (kiff/tafs#107): apart from the
	// agent's runs and chats.
	KindTrial Kind = "trial"
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
	// Owner is who the session is the thread of (#219): the requester who
	// opened it, as run.Requester's Key ("member:<subject>", "door:<id>").
	// Empty for a job's session, and for sessions from before owners. Set
	// once, when the session is opened.
	Owner string `json:"owner,omitempty"`
	// Archived is set when its owner put the chat away (kiff/tafs#82): it
	// leaves their list, and a live message no longer goes on in it. It is
	// kept for its retention all the same.
	Archived bool `json:"archived,omitempty"`
}

// OwnedBy reports whether owner (a requester's Key, or empty for none) may
// read or go on in the session (#219): only its own owner may. One with no
// owner (a job's, or one from before owners) is no requester's, so it is
// never listed to a member of a shared taf; a run with no requester may go
// on in it.
func (s Session) OwnedBy(owner string) bool {
	return s.Owner == owner
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
	// Component is a UI component the agent showed the owner (steps, an
	// approval, a result), drawn by the app in the chat (kiff/agents#91).
	Component EventKind = "component"
	// TrialWrite is a write a trial made and KIFF decided without sending it
	// (kiff/tafs#107): the call and KIFF's decision, kept like the agent's
	// messages so a trial's result outlives its tool calls.
	TrialWrite EventKind = "trial_write"
	// CallRef is what a call through KIFF's gateway acted on: KIFF's key for
	// the call and the value of the argument its Card scopes on (a payment's
	// id), nothing else from the input (kiff/tafs#167). It outlives the
	// call's own events, so the owner's Record can say which payment.
	CallRef EventKind = "call_ref"
	// KeptRead is what a read-only tool call read in a chat, the call and its
	// result together (KeptReadBody), kept for the chat's next runs while it
	// goes on (UntilIdle), sealed (kiff/agents#172). It never leaves the
	// runner: a session's events as the API shows them leave it out.
	KeptRead EventKind = "kept_read"

	// Summary is the agent's summary of a chat's earlier runs, written once
	// they no longer fit its history (SummaryBody): kept like the agent's
	// messages, so the runs it covers needn't be read again.
	Summary EventKind = "summary"
)

// EventKinds lists every kind a Policy must have a rule for, so it can be
// checked for gaps.
var EventKinds = []EventKind{OwnerMessage, AgentMessage, ToolCall, ToolResult, Report, MemoryNote, Usage, Error}

// Defaulted are kinds a Policy may leave out: each takes the rule of the kind
// named here. A kind added later goes here, so a stored policy written before
// it keeps working. A component, a trial's write, a call's reference and a
// summary are kept like the agent's messages. A KeptRead left out is not
// written at all (Policy.Rule).
var Defaulted = map[EventKind]EventKind{Component: AgentMessage, TrialWrite: AgentMessage, CallRef: AgentMessage, Summary: AgentMessage, KeptRead: KeptRead}

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
	// seal: the Recording seals Body before the Store gets it.
	seal bool
}

// Shown is events without what only the runner reads (KeptRead): what any
// view of a session, its API, a stream or an operator's page, may show.
func Shown(events []Event) []Event {
	return slices.DeleteFunc(events, func(e Event) bool { return e.Kind == KeptRead })
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

// KeptReadBody is the body of a KeptRead event, before it is sealed: one
// read-only call and what it returned, as the model saw them.
type KeptReadBody struct {
	CallID string          `json:"call_id"`
	Tool   string          `json:"tool"`
	Input  json.RawMessage `json:"input"`
	Result string          `json:"result"`
}

// SummaryBody is the body of a Summary event: the agent's summary of the
// session's runs up to and including Through, the last run it covers. The
// owner's first message is kept as they wrote it (Opening), whatever the
// summary says of it.
type SummaryBody struct {
	Text    string `json:"text"`
	Opening string `json:"opening,omitempty"`
	Through string `json:"through"`
	Runs    int    `json:"runs"`
}

// TurnTiming is how long one model turn took, on its Usage event, with
// sizes in bytes and never what was said: the whole turn (MS), its first
// streamed content (FirstMS), what it streamed of thinking, text and tool
// input, its tool calls, and the tool calls it waited on. On a run's first turn, StartMS is from the
// run's creation to that turn: the claim, the build, the session's read and
// any cold start. It rides on Usage, which every turn writes anyway, so
// timing a run costs no write of its own.
type TurnTiming struct {
	MS             int64 `json:"ms"`
	FirstMS        int64 `json:"first_ms,omitempty"`
	ThinkingBytes  int   `json:"thinking_bytes,omitempty"`
	TextBytes      int   `json:"text_bytes,omitempty"`
	ToolInputBytes int   `json:"tool_input_bytes,omitempty"`
	ToolCalls      int   `json:"tool_calls,omitempty"`
	StartMS        int64 `json:"start_ms,omitempty"`
	// Waited is the tool calls of the turn before, whose results this turn
	// read: each call's time and result size.
	Waited []ToolTiming `json:"waited,omitempty"`
}

// ToolTiming is one tool call's time, its result's size, and how many calls
// ran beside it in the same turn (Batch, itself included).
type ToolTiming struct {
	Tool        string `json:"tool"`
	MS          int64  `json:"ms"`
	ResultBytes int    `json:"result_bytes,omitempty"`
	Batch       int    `json:"batch,omitempty"`
	IsError     bool   `json:"is_error,omitempty"`
}

// UsageBody is the body of a Usage event: what one model call used and cost.
// Cost is in millionths of a US dollar, so it sums exactly.
type UsageBody struct {
	Model        string `json:"model"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	CacheRead    int64  `json:"cache_read_tokens,omitempty"`
	CacheWrite   int64  `json:"cache_write_tokens,omitempty"`
	// CacheWrite1h is what it wrote to the cache for an hour.
	CacheWrite1h int64 `json:"cache_write_1h_tokens,omitempty"`
	CostMicroUSD int64 `json:"cost_micro_usd"`
	// Context is what the call's input was made of, when the caller knew.
	Context *ContextBody `json:"context,omitempty"`
	// Timing is how long the turn took, when the caller timed it.
	Timing *TurnTiming `json:"timing,omitempty"`
	// Speed is the speed the turn ran at, for an agent that runs fast while
	// someone waits: "fast", or "standard" when it couldn't. Empty for the
	// others. A conversation keeps the speed it began with (SpeedOf).
	Speed string `json:"speed,omitempty"`
}

// The speeds a turn runs at (UsageBody.Speed).
const (
	SpeedFast     = "fast"
	SpeedStandard = "standard"
)

// SpeedOf is the speed a session's turns ran at last: the last Usage that
// says one, or "" when it has no turn yet. A conversation keeps one speed,
// since the provider's cache is per speed: one that began at standard, or
// fell back to it, stays there. Turns that say none (a summary, or turns
// from before speeds were recorded) don't change it; a session with only
// those ran at standard.
func SpeedOf(events []Event) string {
	turns := false
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != Usage {
			continue
		}
		turns = true
		var u UsageBody
		if json.Unmarshal(events[i].Body, &u) == nil && u.Speed != "" {
			return u.Speed
		}
	}
	if turns {
		return SpeedStandard
	}
	return ""
}

// Prompt is the call's whole input, in tokens: what it sent fresh, what the
// cache served and what it wrote to the cache.
func (u UsageBody) Prompt() int64 { return u.InputTokens + u.CacheRead + u.CacheWrite + u.CacheWrite1h }

// Room is what the model's context window had left after the call's input,
// in tokens; ok is false when the window isn't known.
func (u UsageBody) Room() (room int64, ok bool) {
	if u.Context == nil || u.Context.WindowTokens <= 0 {
		return 0, false
	}
	return u.Context.WindowTokens - u.Prompt(), true
}

// ContextBody is what one model call's input was made of: each part's size
// in bytes (ToolResultsBytes is the part of MessagesBytes that tool results
// take), how many messages it held, and the model's context window in
// tokens, 0 when unknown. Its tokens are the UsageBody's.
type ContextBody struct {
	SystemBytes      int   `json:"system_bytes"`
	ToolsBytes       int   `json:"tools_bytes"`
	MessagesBytes    int   `json:"messages_bytes"`
	ToolResultsBytes int   `json:"tool_results_bytes"`
	Messages         int   `json:"messages"`
	WindowTokens     int64 `json:"window_tokens,omitempty"`
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
// isn't open (it ended, or never started), so it takes no events; ErrGap, that
// an Append skipped a Seq; ErrNotPaused, that a run can't resume because it
// isn't held; ErrLate, that a run was created too long ago to start.
var (
	ErrNotFound  = errors.New("session: not found")
	ErrEnded     = errors.New("session: run not open")
	ErrGap       = errors.New("session: events out of sequence")
	ErrNotPaused = errors.New("session: run not paused")
	ErrLate      = errors.New("session: run created too long ago to start")
)

// MaxStartDelay is how long after its creation a run may start recording.
// Past it, Begin refuses the run, so an ended run can never be started again
// even once what a Store keeps of its end has expired.
const MaxStartDelay = time.Hour

// RunState is what a Store knows of one run's recording: the last Seq it
// stored, whether it is open (started and not ended), whether it is held
// (paused), and whether it has ended. Ended is known while the Store keeps a
// trace of the end, at least MaxStartDelay past the run's creation and
// usually as long as the session; an ended run is never open again.
type RunState struct {
	Seq   int64 `json:"seq"`
	Open  bool  `json:"open,omitempty"`
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
	// Begin opens a run in a session, so it can take events. created is when
	// the run was made: a run created more than MaxStartDelay ago is refused
	// (ErrLate), and so is one that ended (ErrEnded). Beginning an open run
	// again changes nothing.
	Begin(ctx context.Context, tenant, session, run string, created time.Time) error
	// Append adds an open run's events, all or none. An event whose Seq the
	// run already reached is skipped, so a retried Append stores nothing
	// twice; any other Seq must follow on without a gap (ErrGap). A run that
	// isn't open takes none (ErrEnded). The Store keeps its own copy of each
	// Body. The session's Updated becomes the last event's Time, and its
	// Expires (unless zero) moves on to the latest event Expires.
	Append(ctx context.Context, tenant, session, run string, events []Event) error
	// Read returns a session and its events, in SortEvents order. The events
	// are the caller's: changing them doesn't change what's stored.
	Read(ctx context.Context, tenant, id string) (Session, []Event, error)
	// ReadRun returns what is recorded of one run of a session, and its
	// events in order. A run with nothing recorded has a zero RunState.
	ReadRun(ctx context.Context, tenant, session, run string) (RunState, []Event, error)
	// State is ReadRun without the events: one cheap read.
	State(ctx context.Context, tenant, session, run string) (RunState, error)
	// List returns an agent's sessions, most recently updated first, at most limit.
	List(ctx context.Context, tenant, agent string, limit int) ([]Session, error)
	// ListOwned is List for one owner: the agent's sessions whose Owner is
	// owner, and those with no owner (from before owners, kept as they
	// were), most recently updated first, at most limit. Never another
	// owner's.
	ListOwned(ctx context.Context, tenant, agent, owner string, limit int) ([]Session, error)
	// Hold marks an open run paused: the session doesn't expire until the run
	// ends. Holding a held run again changes nothing; a run that isn't open
	// can't be held (ErrEnded).
	Hold(ctx context.Context, tenant, session, run string) error
	// EndRun ends a run: it takes no more events, its UntilEnd events are
	// dropped, and its hold is released. The session's Expires (unless zero)
	// moves on to expires, if later: retention starts again when a run ends.
	// Ending a run that isn't open is ErrEnded.
	EndRun(ctx context.Context, tenant, session, run string, expires time.Time) error
	// SetArchived archives the session, or puts it back (Session.Archived).
	// A session that isn't there is ErrNotFound.
	SetArchived(ctx context.Context, tenant, id string, archived bool) error
	// Delete removes a session and its events.
	Delete(ctx context.Context, tenant, id string) error
	// DeleteAgent removes all of an agent's sessions and events.
	DeleteAgent(ctx context.Context, tenant, agent string) error
	// Purge removes events and sessions whose Expires is before now, and
	// returns how many events it removed. A Store whose database expires items
	// by itself may remove nothing here.
	Purge(ctx context.Context, now time.Time) (int, error)
}
