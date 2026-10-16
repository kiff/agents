package memory

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"sync"
	"time"

	"github.com/kiff/agents/run"
)

// Runs is a run.Runs held in memory.
type Runs struct {
	now func() time.Time

	mu   sync.Mutex
	runs map[[2]string][]byte // by tenant and id: the run as JSON, so nothing is shared with callers
	// holds finds a run by tenant and a requirement's exception, for as long
	// as the run exists.
	holds map[[2]string]string
}

// NewRuns returns an empty run store on the system clock.
func NewRuns() *Runs { return NewRunsAt(time.Now) }

// NewRunsAt returns an empty run store whose clock decides what has expired.
func NewRunsAt(now func() time.Time) *Runs {
	return &Runs{now: now, runs: map[[2]string][]byte{}, holds: map[[2]string]string{}}
}

var _ run.Runs = (*Runs)(nil)

// gone reports whether r has ended and expired as of now.
func gone(r run.Run, now time.Time) bool {
	return r.Status.Final() && !r.Expires.IsZero() && r.Expires.Before(now)
}

// get returns the tenant's live run. Called with mu held.
func (m *Runs) get(tenant, id string) (run.Run, bool) {
	raw, ok := m.runs[[2]string{tenant, id}]
	if !ok {
		return run.Run{}, false
	}
	var r run.Run
	if json.Unmarshal(raw, &r) != nil || gone(r, m.now()) {
		return run.Run{}, false
	}
	return r, true
}

func (m *Runs) Create(_ context.Context, r run.Run) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[[2]string{r.Tenant, r.ID}]; ok {
		return run.ErrExists
	}
	m.runs[[2]string{r.Tenant, r.ID}] = raw
	m.index(r)
	return nil
}

// index records r's holds. Called with mu held.
func (m *Runs) index(r run.Run) {
	for _, ex := range run.HoldsOf(r) {
		m.holds[[2]string{r.Tenant, ex}] = r.ID
	}
}

// forget drops the holds of the tenant's run id. Called with mu held.
func (m *Runs) forget(tenant, id string) {
	for k, v := range m.holds {
		if k[0] == tenant && v == id {
			delete(m.holds, k)
		}
	}
}

func (m *Runs) ByHold(_ context.Context, tenant, exception string) (run.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.holds[[2]string{tenant, exception}]
	if !ok {
		return run.Run{}, run.ErrNotFound
	}
	r, ok := m.get(tenant, id)
	if !ok {
		return run.Run{}, run.ErrNotFound
	}
	return r, nil
}

func (m *Runs) Get(_ context.Context, tenant, id string) (run.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.get(tenant, id)
	if !ok {
		return run.Run{}, run.ErrNotFound
	}
	return r, nil
}

func (m *Runs) Put(_ context.Context, prev, next run.Run) error {
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.get(prev.Tenant, prev.ID)
	switch {
	case !ok:
		return run.ErrNotFound
	case !run.Same(stored, next):
		return run.ErrFixed
	case stored.Version != prev.Version || next.Version != prev.Version+1:
		return run.ErrConflict
	}
	m.runs[[2]string{prev.Tenant, prev.ID}] = raw
	m.index(next)
	return nil
}

// all returns the live runs that pass keep, without their paused turn, as
// List and Due promise. Called with mu held.
func (m *Runs) all(keep func(run.Run) bool) []run.Run {
	var out []run.Run
	for k := range m.runs {
		if r, ok := m.get(k[0], k[1]); ok && keep(r) {
			r.Paused = nil
			out = append(out, r)
		}
	}
	return out
}

func (m *Runs) List(_ context.Context, tenant, agent string, limit int) ([]run.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.all(func(r run.Run) bool { return r.Tenant == tenant && r.Agent == agent })
	newestFirst(out)
	return limitTo(out, limit), nil
}

func newestFirst(runs []run.Run) {
	slices.SortFunc(runs, func(a, b run.Run) int { return cmp.Or(b.Created.Compare(a.Created), cmp.Compare(b.ID, a.ID)) })
}

func limitTo(runs []run.Run, limit int) []run.Run {
	if limit > 0 && len(runs) > limit {
		return runs[:limit]
	}
	return runs
}

func (m *Runs) ListAll(_ context.Context, limit int) ([]run.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.all(func(run.Run) bool { return true })
	newestFirst(out)
	return limitTo(out, limit), nil
}

func (m *Runs) ListPaused(_ context.Context, limit int) ([]run.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.all(func(r run.Run) bool { return r.Status == run.Paused })
	newestFirst(out)
	return limitTo(out, limit), nil
}

func (m *Runs) ListSource(_ context.Context, tenant string, trigger run.Trigger, source string, limit int) ([]run.Run, error) {
	if source == "" {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.all(func(r run.Run) bool { return r.Tenant == tenant && r.Trigger == trigger && r.Source == source })
	newestFirst(out)
	return limitTo(out, limit), nil
}

func (m *Runs) Due(_ context.Context, now time.Time, limit int) ([]run.Run, error) {
	return m.due(now, false, limit), nil
}

func (m *Runs) CountActive(_ context.Context, tenant string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k := range m.runs {
		if k[0] != tenant {
			continue // another tenant's: not decoded
		}
		if r, ok := m.get(k[0], k[1]); ok && r.Active() {
			n++
		}
	}
	return n, nil
}

func (m *Runs) DuePaused(_ context.Context, now time.Time, limit int) ([]run.Run, error) {
	return m.due(now, true, limit), nil
}

func (m *Runs) due(now time.Time, paused bool, limit int) []run.Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.all(func(r run.Run) bool {
		d := r.Due()
		return !d.IsZero() && d.Before(now) && (r.Status == run.Paused) == paused
	})
	slices.SortFunc(out, func(a, b run.Run) int { return cmp.Or(a.Due().Compare(b.Due()), cmp.Compare(a.ID, b.ID)) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (m *Runs) Delete(_ context.Context, tenant, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.get(tenant, id); !ok {
		return run.ErrNotFound
	}
	delete(m.runs, [2]string{tenant, id})
	m.forget(tenant, id)
	return nil
}

func (m *Runs) DeleteAgent(_ context.Context, tenant, agent string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.all(func(r run.Run) bool { return r.Tenant == tenant && r.Agent == agent }) {
		delete(m.runs, [2]string{tenant, r.ID})
		m.forget(tenant, r.ID)
	}
	return nil
}
