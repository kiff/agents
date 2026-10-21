package session

import (
	"fmt"
	"time"
)

// Keep is how long an event of a kind lives.
type Keep int

const (
	// Never: the event is not written at all.
	Never Keep = iota
	// UntilEnd: written while its run goes on, so the agent can use it, and
	// dropped when that run ends: one run's events don't stay for the next.
	UntilEnd
	// Kept: kept for the rule's Retention, or as long as the session if zero.
	Kept
	// UntilIdle: kept after its run ends, for the same chat's next runs,
	// until the chat has been idle for the rule's Retention, and never
	// longer than its Max after it was recorded. Only KeptRead takes it, and
	// only in a chat: elsewhere the event isn't written. Its body is sealed
	// with the tenant's key before it is stored (Sealer), and nothing is
	// written without one.
	UntilIdle
)

// Rule is the keep rule for one kind of event. Max applies only to
// UntilIdle.
type Rule struct {
	Keep      Keep
	Retention time.Duration
	Max       time.Duration `json:",omitzero"`
}

// valid reports whether r is a rule kind may have.
func (r Rule) valid(kind EventKind) bool {
	switch {
	case r.Keep < Never || r.Keep > UntilIdle || r.Retention < 0 || r.Max < 0:
		return false
	case r.Keep == UntilIdle:
		return kind == KeptRead && r.Retention > 0 && r.Max >= r.Retention
	case kind == KeptRead && r.Keep == Kept:
		return false // what a tool read is never kept for good
	}
	return r.Max == 0 && (r.Retention == 0 || r.Keep == Kept)
}

// Policy is the keep rule for every kind of event. A policy that leaves a kind
// out is refused, so a new kind is never recorded by accident. A tool call and
// its result share one rule: what is kept of a run never holds one without the
// other.
type Policy map[EventKind]Rule

// Check reports a kind the policy has no rule for, or a nonsense rule.
func (p Policy) Check() error {
	for _, k := range EventKinds {
		r, ok := p[k]
		if !ok {
			return fmt.Errorf("session: policy has no rule for %q", k)
		}
		if r.Keep < Never || r.Keep > UntilIdle {
			return fmt.Errorf("session: rule for %q has an unknown keep %d", k, r.Keep)
		}
		if !r.valid(k) {
			return fmt.Errorf("session: rule for %q: retention applies only to kept events, and is never negative; only %q is kept until idle", k, KeptRead)
		}
	}
	for k := range Defaulted {
		if r, ok := p[k]; ok && !r.valid(k) {
			return fmt.Errorf("session: rule for %q is not a valid rule", k)
		}
	}
	if p[ToolCall] != p[ToolResult] {
		return fmt.Errorf("session: %q and %q must share one rule", ToolCall, ToolResult)
	}
	return nil
}

// Rule is the rule for kind: the policy's own, or for a Defaulted kind it
// leaves out, the rule of the kind it defaults to. A KeptRead the policy
// leaves out isn't written: the tool's call and result are recorded anyway.
func (p Policy) Rule(kind EventKind) Rule {
	if r, ok := p[kind]; ok {
		return r
	}
	if kind == KeptRead {
		return Rule{Keep: Never}
	}
	if d, ok := Defaulted[kind]; ok {
		return p[d]
	}
	return Rule{}
}

// DefaultPolicy keeps tool calls and what they return only while the run
// needs them, since they can carry a company's customer data, and everything
// else for retention.
func DefaultPolicy(retention time.Duration) Policy {
	p := Policy{}
	for _, k := range EventKinds {
		p[k] = Rule{Keep: Kept, Retention: retention}
	}
	p[ToolCall] = Rule{Keep: UntilEnd}
	p[ToolResult] = Rule{Keep: UntilEnd}
	return p
}

// SessionRetention is how long a session lives after its last event: the
// longest retention of a kept kind. It is zero (kept until deleted) when any
// kept kind has no retention.
func (p Policy) SessionRetention() time.Duration {
	var longest time.Duration
	for _, r := range p {
		if r.Keep != Kept {
			continue
		}
		if r.Retention == 0 {
			return 0
		}
		longest = max(longest, r.Retention)
	}
	return longest
}
