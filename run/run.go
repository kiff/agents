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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

// Trigger is what started a run.
type Trigger string

const (
	ByOwner    Trigger = "owner"    // the owner sent a message
	BySchedule Trigger = "schedule" // one of the agent's schedules fired
	ByEvent    Trigger = "event"    // work came in: a payment event, an e-invoice
	ByEval     Trigger = "eval"     // a case of an eval run
)

// Run is one execution of an agent, as stored.
type Run struct {
	ID      string  `json:"id"`
	Tenant  string  `json:"tenant"`
	Agent   string  `json:"agent"`
	Session string  `json:"session"`
	Trigger Trigger `json:"trigger"`
	// Source is what started the run within its trigger: the schedule's ID
	// for a scheduled run, the event's for one an event started; empty for
	// the owner's runs and evals'. ListSource lists the runs one source
	// started.
	Source string `json:"source,omitempty"`
	Status Status `json:"status"`
	// Input is what the run was asked: the owner's message, or the job's
	// instruction. A PENDING run is started from it.
	Input string `json:"input"`

	// Stop is why a COMPLETED run stopped: the agent answered, or a cap
	// (cost, turns, output length) stopped it.
	Stop string `json:"stop,omitempty"`
	// Output is the agent's last answer.
	Output string `json:"output,omitempty"`
	// Error says what went wrong, for an ERROR run.
	Error string `json:"error,omitempty"`
	// Cost is what the run's model turns cost so far, in millionths of a US
	// dollar.
	Cost int64 `json:"cost_micro_usd"`
	// Turns counts the run's model turns so far, across its pauses.
	Turns int `json:"turns"`

	// Requirements are what a PAUSED run waits for: one per call KIFF holds.
	Requirements []Requirement `json:"requirements,omitempty"`
	// Paused is the turn the run paused in, to carry on from.
	Paused *Turn `json:"paused,omitempty"`

	// Cancel records that the run was asked to stop: a running run stops at
	// its next checkpoint (before a model turn, after a tool batch).
	Cancel bool `json:"cancel,omitempty"`

	// Claim names the invocation running the run, and Lease is when its claim
	// ends: a minute after that invocation's own deadline. Every write the run
	// makes checks it still holds both. Zero unless RUNNING.
	Claim string    `json:"claim,omitempty"`
	Lease time.Time `json:"lease,omitzero"`

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
	// Operation is KIFF's operation ID for the call: how its outcome is
	// collected from the gateway, and how the hold is withdrawn.
	Operation string `json:"operation"`
	// Expires is when KIFF's hold expires. Expiry means refused: once it has
	// passed with no answer recorded, KIFF is asked for the call's outcome.
	Expires time.Time `json:"expires"`
	// Answered is when KIFF reported the hold answered; zero until then.
	Answered time.Time `json:"answered,omitzero"`
}

// Resolved reports whether KIFF has resolved the requirement as of now: it
// was answered, or its hold has expired.
func (r Requirement) Resolved(now time.Time) bool {
	return !r.Answered.IsZero() || !now.Before(r.Expires)
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
