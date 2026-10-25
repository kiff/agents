// Package run is one execution of an agent: one owner message, or one
// scheduled or triggered job. A run has a stable ID and a status for its whole
// life, through any number of pauses, and lives in a session, the thread of
// runs it belongs to. A run pauses when KIFF holds at least one of its calls;
// what it waits for are its requirements, one per held call.
//
// Requirements and the turn a run paused in belong to the run, not to its
// events, so the keep rules never drop them before the run ends: continuing
// never depends on what was kept.
package run

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/kiff/agents/model"
)

// Status is where a run is. COMPLETED, ERROR and CANCELLED are final.
type Status string

const (
	Pending   Status = "PENDING"   // stored, not yet claimed by an invocation
	Running   Status = "RUNNING"   // claimed, under a lease
	Paused    Status = "PAUSED"    // KIFF holds at least one of its calls
	Completed Status = "COMPLETED" // the agent answered, or a cap stopped it; Stop says which
	Error     Status = "ERROR"     // it failed, its lease ran out, or no invocation claimed it in time
	Cancelled Status = "CANCELLED" // cancelled; it doesn't undo what a tool already did
)

// Final reports whether a run in this status can change no more.
func (s Status) Final() bool { return s == Completed || s == Error || s == Cancelled }

// Requester is who asked for a run (#219): a member of the account, a
// door's sender, or a manager acting on someone's work. The agent acts on
// its own Cards whoever asks; the requester is whose thread the run is in,
// and who it is attributed to. The caller names it, from the person's own
// signed-in session or the door's checked sender.
type Requester struct {
	Kind RequesterKind `json:"kind"`
	ID   string        `json:"id"`
}

// RequesterKind is what kind of requester asked.
type RequesterKind string

const (
	ByMember  RequesterKind = "member"  // a person on the account, by their KIFF subject
	ByDoor    RequesterKind = "door"    // a door's sender, by the door's id for them
	ByManager RequesterKind = "manager" // a manager acting on someone's work, by their KIFF subject
	// ByAgent is another agent of the account asking on its own, by its
	// agent id: Coach handing a taf a task. Its chats are its own, so no
	// person lists them, and it isn't a person the account is asked about.
	ByAgent RequesterKind = "agent"
)

// requesterKey keys a run's requester on a context.
type requesterKey struct{}

// WithRequester marks ctx with the run's requester: set by the runner from
// the stored run, never by the model, so a tool acts for whoever asked
// (#219).
func WithRequester(ctx context.Context, q Requester) context.Context {
	return context.WithValue(ctx, requesterKey{}, q)
}

// RequesterFrom is the requester ctx is marked with, and whether it is.
func RequesterFrom(ctx context.Context) (Requester, bool) {
	q, ok := ctx.Value(requesterKey{}).(Requester)
	return q, ok
}

// Ref names the run a tool is called in: its session and its ID. Set by the
// runner, never by the model.
type Ref struct{ Session, Run string }

type refKey struct{}

// WithRef marks ctx with the run it belongs to.
func WithRef(ctx context.Context, r Ref) context.Context {
	return context.WithValue(ctx, refKey{}, r)
}

// RefFrom is the run ctx is marked with, and whether it is.
func RefFrom(ctx context.Context) (Ref, bool) {
	r, ok := ctx.Value(refKey{}).(Ref)
	return r, ok
}

// MaxRequesterID is the longest requester ID.
const MaxRequesterID = 200

// Key is the requester as a session's owner: "member:<subject>". Empty for
// no requester.
func (q Requester) Key() string {
	if q.ID == "" {
		return ""
	}
	return string(q.Kind) + ":" + q.ID
}

// Valid reports whether q names a requester of a known kind. An ID has no
// ":" (a key's separator) and no "|" (the stores' separator in keys).
func (q Requester) Valid() bool {
	switch q.Kind {
	case ByMember, ByDoor, ByManager, ByAgent:
	default:
		return false
	}
	return q.ID != "" && len(q.ID) <= MaxRequesterID && !strings.ContainsAny(q.ID, ":|\n")
}

// Trigger is what started a run.
type Trigger string

const (
	ByOwner    Trigger = "owner"    // the owner sent a message
	BySchedule Trigger = "schedule" // one of the agent's schedules fired
	ByEvent    Trigger = "event"    // work came in: a payment event, an e-invoice
	ByEval     Trigger = "eval"     // a case of an eval run
	// ByTrial is a trial of a version that may not be live (kiff/tafs#107):
	// its writes go to KIFF under KIFF-Trial, it writes no notes, and its
	// session is a trial's, apart from the agent's runs and chats. KIFF
	// decides them and sends none, unless the draft is a trial Card the
	// owner started trying (kiff/tafs#173): then they are sent, decided on
	// that Card alone.
	ByTrial Trigger = "trial"
)

// Run is one execution of an agent, as stored.
type Run struct {
	ID      string  `json:"id"`
	Tenant  string  `json:"tenant"`
	Agent   string  `json:"agent"`
	Session string  `json:"session"`
	Trigger Trigger `json:"trigger"`
	// Requester is who asked for the run (#219); zero for a run no one
	// asked for (a schedule's, an event's) or from before requesters.
	Requester Requester `json:"requester,omitzero"`
	// NewSession is whether the run opened its session: with a live chat,
	// the server picks the session, and the caller learns from this whether
	// the message began a new one (#219).
	NewSession bool `json:"new_session,omitempty"`
	// Source is what started the run within its trigger: the schedule's ID
	// for a scheduled run, the event's for one an event started; empty for
	// the owner's runs and evals'. ListSource lists the runs one source
	// started.
	Source string `json:"source,omitempty"`
	Status Status `json:"status"`
	// Input is what the run was asked: the owner's message, or the job's
	// instruction. A PENDING run is started from it.
	Input string `json:"input"`

	// Stop is why a COMPLETED run stopped: the agent answered, a cap (cost,
	// turns, output length) stopped it, or the model's turn was refused.
	Stop string `json:"stop,omitempty"`
	// Refusal is the category a refused run's turn was declined for ("cyber",
	// "bio", …), when the provider named one.
	Refusal string `json:"refusal,omitempty"`
	// Output is the agent's last answer.
	Output string `json:"output,omitempty"`
	// Error says what went wrong, for an ERROR run.
	Error string `json:"error,omitempty"`
	// AgentVersion is the agent's version the run ran (kiff/tafs#107): the
	// live one when it started, or the version a trial tries. 0 for an agent
	// with no versions.
	AgentVersion int `json:"agent_version,omitempty"`
	// Draft is the KIFF draft a trial's writes are decided against (its
	// proposed Cards); empty decides them against the live Cards. Only a
	// trial has one.
	Draft string `json:"draft,omitempty"`

	// Cost is what the run's model turns cost so far, in millionths of a US
	// dollar.
	Cost int64 `json:"cost_micro_usd"`
	// Meter is the billing period the run's cost counts toward for AI fair
	// use (kiff/tafs#193), as the caller named it when it started a chat;
	// empty for a run that isn't counted.
	Meter string `json:"meter,omitempty"`
	// Notify is where the run is announced when it ends or pauses
	// (Notifying): set when it starts, never changed. Zero: nowhere.
	Notify Notify `json:"notify,omitzero"`
	// Turns counts the run's model turns so far, across its pauses.
	Turns int `json:"turns"`

	// Requirements are what a PAUSED run waits for: one per call KIFF holds.
	Requirements []Requirement `json:"requirements,omitempty"`
	// Paused is the turn the run paused in, to carry on from.
	Paused *Turn `json:"paused,omitempty"`
	// Holds are the exceptions of every call KIFF has held in the run, over
	// all its pauses: what Runs.ByHold finds it by, for as long as the run
	// exists. Change keeps it; it only grows.
	Holds []string `json:"holds,omitempty"`

	// Cancel records that the run was asked to stop: a running run stops at
	// its next checkpoint (before a model turn, after a tool batch).
	Cancel bool `json:"cancel,omitempty"`
	// Steering is what the owner wrote to the run while it went (#102). The
	// run takes each message at its next checkpoint, as it would a cancel,
	// and records it then as an owner message. One it never took, because it
	// ended first, stays here not taken: the caller sends it as the next
	// message, so nothing the owner wrote is dropped.
	Steering []Steering `json:"steering,omitempty"`

	// Claim names the invocation running the run, and Lease is when its claim
	// ends: a minute after that invocation's own deadline. Every write the run
	// makes checks it still holds both. Zero unless RUNNING.
	Claim string    `json:"claim,omitempty"`
	Lease time.Time `json:"lease,omitzero"`

	// Retries counts the sweep's continues of a resolved PAUSED run, and
	// Retry is when the sweep looks at the run again: each continue doubles
	// the wait from ResolvedGrace, up to MaxRetryWait, so a run that stays
	// paused backs off and leaves the front of the sweep. A new pause starts
	// both from zero.
	Retries int       `json:"retries,omitempty"`
	Retry   time.Time `json:"retry,omitzero"`

	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	// Expires is when a run that has ended may be removed (zero: kept until
	// deleted). It comes from the agent's keep rules; a run that hasn't
	// ended never expires.
	Expires time.Time `json:"expires,omitzero"`
	// Version counts the run's stored changes. Every change is a
	// compare-and-set on it.
	Version int64 `json:"version"`
}

// Requirement is one call KIFF holds for the owner: the call itself, KIFF's
// operation ID for it, and when the hold expires. KIFF resolves it, only
// KIFF: an answer is recorded from KIFF's signed alert or by asking KIFF,
// never from a request that claims it.
type Requirement struct {
	// CallID is the model's tool-use ID for the call, Tool and Input the call
	// as the model made it.
	CallID string          `json:"call_id"`
	Tool   string          `json:"tool"`
	Input  json.RawMessage `json:"input"`
	// Operation is the call's kiff_operation_id: repeating the identical
	// call with it collects the outcome from the gateway.
	Operation string `json:"operation"`
	// Exception is KIFF's ID for the owner's decision on the hold: what its
	// hold_answered alert names, and what withdrawing the call takes.
	Exception string `json:"exception,omitempty"`
	// Expires is when KIFF's hold expires. Expiry means refused: once it has
	// passed with no answer recorded, KIFF is asked for the call's outcome.
	Expires time.Time `json:"expires"`
	// Answered is when KIFF reported the hold answered; zero until then.
	Answered time.Time `json:"answered,omitzero"`
	// Answer is what KIFF reported: approved, refused, expired, or
	// invalidated (withdrawn). The outcome itself is collected from the
	// gateway (Operation).
	Answer string `json:"answer,omitempty"`
}

// Resolved reports whether KIFF has resolved the requirement as of now: it
// was answered, or its hold has expired. A requirement with no expiry and no
// answer is not resolved: it fails closed, so a run paused before KIFF's
// expiry was recorded waits, visibly, rather than continuing unanswered.
func (r Requirement) Resolved(now time.Time) bool {
	return !r.Answered.IsZero() || (!r.Expires.IsZero() && !now.Before(r.Expires))
}

// Steering is one message the owner wrote to a run while it went.
type Steering struct {
	Text string    `json:"text"`
	At   time.Time `json:"at"`
	// Taken is set when the run took the message, at a checkpoint.
	Taken bool `json:"taken,omitempty"`
	// Requester is who wrote it, if the caller said (#219): their flood
	// guard counts it.
	Requester Requester `json:"requester,omitzero"`
}

// Taken is the text of each message the run has taken, in order.
func (r Run) Taken() []string { return r.steering(true) }

// Untaken is the text of each message the run hasn't taken (yet), in order.
func (r Run) Untaken() []string { return r.steering(false) }

func (r Run) steering(taken bool) []string {
	var out []string
	for _, s := range r.Steering {
		if s.Taken == taken {
			out = append(out, s.Text)
		}
	}
	return out
}

// Turn is the turn a run paused in: the model's message, with its tool calls,
// and the results of the calls KIFF didn't hold, in the order of the calls.
// The held calls' results are collected from KIFF when the run continues.
type Turn struct {
	Reply   model.Message `json:"reply"`
	Results []model.Block `json:"results,omitempty"`
}

// NewID returns a new run ID.
func NewID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b)
}
