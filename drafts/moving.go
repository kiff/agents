package drafts

import (
	"context"
	"errors"
)

// Leaving is where records are leaving from: read, and deleted once the
// new store holds the draft, never written.
type Leaving interface {
	Get(ctx context.Context, tenant, draft string) (Record, error)
	// Delete removes the draft's record; one that isn't there is no error.
	Delete(ctx context.Context, tenant, draft string) error
}

// Moving is a Store moving to New: it writes only to New, and reads New, then
// Old for a record written before the move. Old is never written, so nothing
// new lands where the records are leaving from; once New holds a draft, its
// record in Old is deleted, so it can never be read back in New's place,
// whatever becomes of New's (its time up, removed by the table's TTL). Once
// every record Old holds has outlived its use, Old goes.
type Moving struct {
	New Store
	Old Leaving
	// Log, if set, hears of an old record that couldn't be deleted. The
	// write still stands: New's record is the draft's, and the clean-up run
	// takes what's left.
	Log func(msg string, err error)
}

var _ Store = Moving{}

// Put writes r to New only, then deletes the draft's record in Old.
func (m Moving) Put(ctx context.Context, tenant, draft string, r Record) error {
	if err := m.New.Put(ctx, tenant, draft, r); err != nil {
		return err
	}
	if m.Old != nil {
		if err := m.Old.Delete(ctx, tenant, draft); err != nil && m.Log != nil {
			m.Log("drafts: the old record wasn't deleted", err)
		}
	}
	return nil
}

// Get reads New, then Old when New has no record at all. A record New
// had, past its time (ErrGone), is gone: the draft's older secret is never
// read back in its place, or a draft claimed after the move would read as
// unclaimed once its record expired.
func (m Moving) Get(ctx context.Context, tenant, draft string) (Record, error) {
	r, err := m.New.Get(ctx, tenant, draft)
	if errors.Is(err, ErrNotFound) && !errors.Is(err, ErrGone) && m.Old != nil {
		return m.Old.Get(ctx, tenant, draft)
	}
	return r, err
}

// List lists New's records only: a record still in Old was written before
// the move, and is past any use a list has.
func (m Moving) List(ctx context.Context, tenant string) (map[string]Record, error) {
	l, ok := m.New.(Lister)
	if !ok {
		return nil, ErrNoList
	}
	return l.List(ctx, tenant)
}
