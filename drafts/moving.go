package drafts

import (
	"context"
	"errors"
)

// Getter reads a draft's record and writes none.
type Getter interface {
	Get(ctx context.Context, tenant, draft string) (Record, error)
}

// Moving is a Store moving to New: it writes only to New, and reads New, then
// Old for a record written before the move. Old is read, never written, so
// nothing new lands where the records are leaving from. Once every record
// Old holds has outlived its use (an unclaimed draft past KIFF's claim
// window, a claimed one past its retries), Old goes.
type Moving struct {
	New Store
	Old Getter
}

var _ Store = Moving{}

// Put writes r to New only.
func (m Moving) Put(ctx context.Context, tenant, draft string, r Record) error {
	return m.New.Put(ctx, tenant, draft, r)
}

// Get reads New, then Old when New has no record.
func (m Moving) Get(ctx context.Context, tenant, draft string) (Record, error) {
	r, err := m.New.Get(ctx, tenant, draft)
	if errors.Is(err, ErrNotFound) && m.Old != nil {
		return m.Old.Get(ctx, tenant, draft)
	}
	return r, err
}
