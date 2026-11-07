package memory

import (
	"context"
	"sync"
	"time"

	"github.com/kiff/agents/usage"
)

// Usage is a usage.Store held in memory. It keeps periods past their until:
// nothing in memory outlives the process anyway.
type Usage struct {
	mu sync.Mutex
	p  map[[2]string]usage.Period // by tenant, period
}

// NewUsage returns an empty usage store.
func NewUsage() *Usage { return &Usage{p: map[[2]string]usage.Period{}} }

var _ usage.Store = (*Usage)(nil)

// Add adds microUSD to the period.
func (u *Usage) Add(_ context.Context, tenant, period string, microUSD int64, _ time.Time) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	p := u.p[[2]string{tenant, period}]
	p.Spent += microUSD
	u.p[[2]string{tenant, period}] = p
	return nil
}

// Reserve adds microUSD only while the period's total stays within limit.
func (u *Usage) Reserve(_ context.Context, tenant, period string, microUSD, limit int64, _ time.Time) (bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	p := u.p[[2]string{tenant, period}]
	if p.Spent+microUSD > limit {
		return false, nil
	}
	p.Spent += microUSD
	u.p[[2]string{tenant, period}] = p
	return true, nil
}

// Get returns the period.
func (u *Usage) Get(_ context.Context, tenant, period string) (usage.Period, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.p[[2]string{tenant, period}], nil
}

// EraseTenant deletes the tenant's periods.
func (u *Usage) EraseTenant(_ context.Context, tenant string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	for k := range u.p {
		if k[0] == tenant {
			delete(u.p, k)
		}
	}
	return nil
}
