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
	// UntilEnd: written while the session runs, so the agent can use it, and
	// dropped when the session ends.
	UntilEnd
	// Kept: kept for the rule's Retention, or as long as the session if zero.
	Kept
)

// Rule is the keep rule for one kind of event.
type Rule struct {
	Keep      Keep
	Retention time.Duration
}

// Policy is the keep rule for every kind of event. A policy that leaves a kind
// out is refused, so a new kind is never recorded by accident.
type Policy map[EventKind]Rule

// Check reports a kind the policy has no rule for, or a nonsense rule.
func (p Policy) Check() error {
	for _, k := range EventKinds {
		r, ok := p[k]
		if !ok {
			return fmt.Errorf("session: policy has no rule for %q", k)
		}
		if r.Keep < Never || r.Keep > Kept {
			return fmt.Errorf("session: rule for %q has an unknown keep %d", k, r.Keep)
		}
		if r.Retention < 0 || (r.Retention > 0 && r.Keep != Kept) {
			return fmt.Errorf("session: rule for %q: retention applies only to kept events, and is never negative", k)
		}
	}
	return nil
}

// DefaultPolicy keeps what tools return only while the run needs it, since it
// can carry a company's customer data, and everything else for retention.
func DefaultPolicy(retention time.Duration) Policy {
	p := Policy{}
	for _, k := range EventKinds {
		p[k] = Rule{Keep: Kept, Retention: retention}
	}
	p[ToolResult] = Rule{Keep: UntilEnd}
	return p
}
