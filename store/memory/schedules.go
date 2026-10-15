package memory

import (
	"cmp"
	"context"
	"slices"
	"sync"

	"github.com/kiff/agents/schedule"
)

// Schedules is a schedule.Schedules held in memory.
type Schedules struct {
	mu   sync.Mutex
	byID map[[2]string]schedule.Schedule // by tenant, id
}

// NewSchedules returns an empty schedule store.
func NewSchedules() *Schedules { return &Schedules{byID: map[[2]string]schedule.Schedule{}} }

var _ schedule.Schedules = (*Schedules)(nil)

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
	return nil
}
