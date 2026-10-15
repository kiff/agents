package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Recorder writes an agent's sessions to a Store under a Policy. The rule is
// applied when an event is written: a Never kind doesn't reach the Store, so
// it can't outlive a crash either.
type Recorder struct {
	store  Store
	policy Policy
	now    func() time.Time
}

// NewRecorder checks the policy and returns a recorder for store.
func NewRecorder(store Store, policy Policy) (*Recorder, error) {
	return NewRecorderAt(store, policy, time.Now)
}

// NewRecorderAt is NewRecorder with its own clock, for tests and replays.
func NewRecorderAt(store Store, policy Policy, now func() time.Time) (*Recorder, error) {
	if err := policy.Check(); err != nil {
		return nil, err
	}
	return &Recorder{store: store, policy: policy, now: now}, nil
}

// Start creates a running session for an agent.
func (r *Recorder) Start(ctx context.Context, tenant, agent string, kind Kind) (Session, error) {
	now := r.now().UTC()
	s := Session{ID: newID(), Tenant: tenant, Agent: agent, Kind: kind, Status: Running, Started: now, Updated: now}
	if err := r.store.Create(ctx, s); err != nil {
		return Session{}, err
	}
	return s, nil
}

// Record writes one event to a running session, as its kind's rule says.
// written is false when the rule says Never.
func (r *Recorder) Record(ctx context.Context, s Session, kind EventKind, body any) (e Event, written bool, err error) {
	rule := r.policy[kind]
	if rule.Keep == Never {
		return Event{}, false, nil
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Event{}, false, err
	}
	now := r.now().UTC()
	e = Event{Session: s.ID, Time: now, Kind: kind, Body: raw, UntilEnd: rule.Keep == UntilEnd}
	if rule.Keep == Kept && rule.Retention > 0 {
		e.Expires = now.Add(rule.Retention)
	}
	e, err = r.store.Append(ctx, s.Tenant, e)
	return e, err == nil, err
}

// End ends a session. Events kept only until the end are dropped with it.
func (r *Recorder) End(ctx context.Context, s Session, status Status) error {
	return r.store.End(ctx, s.Tenant, s.ID, status)
}

// Cost sums the cost of a session's Usage events, in millionths of a dollar.
func Cost(events []Event) (int64, error) {
	var total int64
	for _, e := range events {
		if e.Kind != Usage {
			continue
		}
		var u UsageBody
		if err := json.Unmarshal(e.Body, &u); err != nil {
			return 0, err
		}
		total += u.CostMicroUSD
	}
	return total, nil
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b)
}
