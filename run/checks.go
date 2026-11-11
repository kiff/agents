package run

import (
	"context"
	"sync"
)

// Checks are the steps handed to the owner that the owner answered "done" in
// this run, each waiting for the run's first read of the vendor about the same
// case: that read is the step's check (KIFF's POST /v1/me/handoffs/{id}/check).
// One per run's execution, shared by its tools through the context: another
// run's read never checks this run's step.
type Checks struct {
	mu      sync.Mutex
	waiting []CheckWait
}

// CheckWait is one step waiting for its check: the case it's about, and how
// its check is posted, with the read's operation ID.
type CheckWait struct {
	Handoff string
	About   string
	Post    func(ctx context.Context, operation string) error
}

// Await notes a step waiting for its check. A step without a case is never
// checked by a read: no read can be said to be about it. A step already
// waiting isn't noted twice.
func (c *Checks) Await(w CheckWait) {
	if c == nil || w.About == "" || w.Handoff == "" || w.Post == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, have := range c.waiting {
		if have.Handoff == w.Handoff {
			return
		}
	}
	c.waiting = append(c.waiting, w)
}

// Take removes and returns the first step waiting on a read about this case:
// each step is checked once, by one read.
func (c *Checks) Take(about string) (CheckWait, bool) {
	if c == nil || about == "" {
		return CheckWait{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, w := range c.waiting {
		if w.About == about {
			c.waiting = append(c.waiting[:i], c.waiting[i+1:]...)
			return w, true
		}
	}
	return CheckWait{}, false
}

type checksKey struct{}

// WithChecks marks ctx with the run's checks.
func WithChecks(ctx context.Context, c *Checks) context.Context {
	return context.WithValue(ctx, checksKey{}, c)
}

// ChecksFrom is the run's checks, or nil outside a run.
func ChecksFrom(ctx context.Context) *Checks {
	c, _ := ctx.Value(checksKey{}).(*Checks)
	return c
}
