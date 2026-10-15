package session

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"
)

// Memory is a Store and Notes held in memory, for tests and local runs.
type Memory struct {
	mu       sync.Mutex
	sessions map[string]*memSession // by session id
	notes    map[[3]string]Note     // by tenant, agent, name
}

type memSession struct {
	s      Session
	events []Event
	seq    int64
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{sessions: map[string]*memSession{}, notes: map[[3]string]Note{}}
}

var (
	_ Store = (*Memory)(nil)
	_ Notes = (*Memory)(nil)
)

func (m *Memory) Create(_ context.Context, s Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.ID] = &memSession{s: s}
	return nil
}

// get returns the tenant's session, or nil: another tenant's is not found.
func (m *Memory) get(tenant, id string) *memSession {
	ms := m.sessions[id]
	if ms == nil || ms.s.Tenant != tenant {
		return nil
	}
	return ms
}

func (m *Memory) Append(_ context.Context, tenant string, e Event) (Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms := m.get(tenant, e.Session)
	if ms == nil {
		return Event{}, ErrNotFound
	}
	if ms.s.Status != Running {
		return Event{}, ErrEnded
	}
	ms.seq++
	e.Seq = ms.seq
	ms.events = append(ms.events, e)
	ms.s.Updated = e.Time
	return e, nil
}

func (m *Memory) Read(_ context.Context, tenant, id string) (Session, []Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms := m.get(tenant, id)
	if ms == nil {
		return Session{}, nil, ErrNotFound
	}
	return ms.s, slices.Clone(ms.events), nil
}

func (m *Memory) List(_ context.Context, tenant, agent string, limit int) ([]Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Session
	for _, ms := range m.sessions {
		if ms.s.Tenant == tenant && ms.s.Agent == agent {
			out = append(out, ms.s)
		}
	}
	slices.SortFunc(out, func(a, b Session) int { return cmp.Or(b.Updated.Compare(a.Updated), cmp.Compare(a.ID, b.ID)) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) End(_ context.Context, tenant, id string, status Status) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms := m.get(tenant, id)
	if ms == nil {
		return ErrNotFound
	}
	if ms.s.Status != Running {
		return ErrEnded
	}
	ms.s.Status = status
	ms.events = slices.DeleteFunc(ms.events, func(e Event) bool { return e.UntilEnd })
	return nil
}

func (m *Memory) Delete(_ context.Context, tenant, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.get(tenant, id) == nil {
		return ErrNotFound
	}
	delete(m.sessions, id)
	return nil
}

func (m *Memory) DeleteAgent(_ context.Context, tenant, agent string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, ms := range m.sessions {
		if ms.s.Tenant == tenant && ms.s.Agent == agent {
			delete(m.sessions, id)
		}
	}
	return nil
}

func (m *Memory) Purge(_ context.Context, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, ms := range m.sessions {
		before := len(ms.events)
		ms.events = slices.DeleteFunc(ms.events, func(e Event) bool { return !e.Expires.IsZero() && e.Expires.Before(now) })
		n += before - len(ms.events)
	}
	return n, nil
}

func (m *Memory) ListNotes(_ context.Context, tenant, agent string) ([]Note, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Note
	for k, n := range m.notes {
		if k[0] == tenant && k[1] == agent {
			out = append(out, n)
		}
	}
	slices.SortFunc(out, func(a, b Note) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

func (m *Memory) PutNote(_ context.Context, n Note) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notes[[3]string{n.Tenant, n.Agent, n.Name}] = n
	return nil
}

func (m *Memory) DeleteNote(_ context.Context, tenant, agent, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.notes, [3]string{tenant, agent, name})
	return nil
}

func (m *Memory) DeleteNotes(_ context.Context, tenant, agent string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.notes {
		if k[0] == tenant && k[1] == agent {
			delete(m.notes, k)
		}
	}
	return nil
}
