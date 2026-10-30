package memory

import (
	"cmp"
	"context"
	"slices"
	"sync"

	"github.com/kiff/agents/schedule"
)

// Schedules is a schedule.Schedules held in memory, and its firings'
// schedule.History.
type Schedules struct {
	mu    sync.Mutex
	byID  map[[2]string]schedule.Schedule // by tenant, id
	fired map[[2]string][]schedule.Fired  // by tenant, id: newest first
}

// NewSchedules returns an empty schedule store.
func NewSchedules() *Schedules {
	return &Schedules{byID: map[[2]string]schedule.Schedule{}, fired: map[[2]string][]schedule.Fired{}}
}

var (
	_ schedule.Schedules = (*Schedules)(nil)
	_ schedule.History   = (*Schedules)(nil)
)

// Add keeps f, replacing the firing due at the same time.
func (m *Schedules) Add(_ context.Context, tenant, id string, f schedule.Fired) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := [2]string{tenant, id}
	list := slices.DeleteFunc(m.fired[k], func(x schedule.Fired) bool { return x.Due.Equal(f.Due) })
	list = append(list, f)
	slices.SortFunc(list, func(x, y schedule.Fired) int { return y.Due.Compare(x.Due) })
	if len(list) > schedule.MaxHistory {
		list = list[:schedule.MaxHistory]
	}
	m.fired[k] = list
	return nil
}

func (m *Schedules) Firings(_ context.Context, tenant, id string, limit int) ([]schedule.Fired, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := m.fired[[2]string{tenant, id}]
	if limit <= 0 || limit > schedule.MaxHistory {
		limit = schedule.MaxHistory
	}
	return slices.Clone(list[:min(limit, len(list))]), nil
}

func (m *Schedules) Put(_ context.Context, s schedule.Schedule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byID[[2]string{s.Tenant, s.ID}] = s
	return nil
}

func (m *Schedules) Get(_ context.Context, tenant, id string) (schedule.Schedule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byID[[2]string{tenant, id}]
	if !ok {
		return schedule.Schedule{}, schedule.ErrNotFound
	}
	return s, nil
}

func (m *Schedules) List(_ context.Context, tenant string) ([]schedule.Schedule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []schedule.Schedule
	for k, s := range m.byID {
		if k[0] == tenant {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(x, y schedule.Schedule) int { return cmp.Compare(x.ID, y.ID) })
	return out, nil
}

func (m *Schedules) Delete(_ context.Context, tenant, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.byID, [2]string{tenant, id})
	delete(m.fired, [2]string{tenant, id})
	return nil
}
