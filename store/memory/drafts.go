package memory

import (
	"context"
	"sync"

	"github.com/kiff/agents/drafts"
)

// Drafts is drafts.Store and drafts.Keys held in memory, for tests and
// local runs.
type Drafts struct {
	mu      sync.Mutex
	records map[[2]string]drafts.Record // by tenant, draft
	keys    map[[2]string]string        // by tenant, ref
}

// NewDrafts returns empty draft records and keys.
func NewDrafts() *Drafts {
	return &Drafts{records: map[[2]string]drafts.Record{}, keys: map[[2]string]string{}}
}

var (
	_ drafts.Store = (*Drafts)(nil)
	_ drafts.Keys  = (*Drafts)(nil)
)

func (d *Drafts) Put(_ context.Context, tenant, draft string, r drafts.Record) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.records[[2]string{tenant, draft}] = r
	return nil
}

func (d *Drafts) Get(_ context.Context, tenant, draft string) (drafts.Record, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, ok := d.records[[2]string{tenant, draft}]
	if !ok {
		return drafts.Record{}, drafts.ErrNotFound
	}
	return r, nil
}

func (d *Drafts) Key(_ context.Context, tenant, ref string) (string, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	k, ok := d.keys[[2]string{tenant, ref}]
	return k, ok, nil
}

func (d *Drafts) Create(_ context.Context, tenant, ref, key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.keys[[2]string{tenant, ref}]; ok {
		return drafts.ErrTaken
	}
	d.keys[[2]string{tenant, ref}] = key
	return nil
}
