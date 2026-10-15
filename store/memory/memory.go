// Package memory is a session store held in memory, for tests and local runs.
package memory

import (
	"bytes"
	"cmp"
	"context"
	"slices"
	"sync"
	"time"

	"github.com/kiff/agents/session"
)

// Store is a session.Store and session.Notes held in memory.
type Store struct {
	now func() time.Time

	mu       sync.Mutex
	sessions map[string]*memSession     // by session id
	notes    map[[3]string]session.Note // by tenant, agent, name
}

type memSession struct {
	s      session.Session
	events []session.Event
	seq    int64 // the last Seq stored, even if its event is gone since
}

// New returns an empty store on the system clock.
func New() *Store { return NewAt(time.Now) }

// NewAt returns an empty store whose clock decides what has expired.
func NewAt(now func() time.Time) *Store {
	return &Store{now: now, sessions: map[string]*memSession{}, notes: map[[3]string]session.Note{}}
}

var (
	_ session.Store = (*Store)(nil)
	_ session.Notes = (*Store)(nil)
)

func expired(at, now time.Time) bool { return !at.IsZero() && at.Before(now) }

func (m *Store) Create(_ context.Context, s session.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.ID] = &memSession{s: s}
	return nil
}

// get returns the tenant's live session, or nil: another tenant's, or an
// expired one, is not found.
func (m *Store) get(tenant, id string) *memSession {
	ms := m.sessions[id]
	if ms == nil || ms.s.Tenant != tenant || expired(ms.s.Expires, m.now()) {
		return nil
	}
	return ms
}

func (m *Store) Append(_ context.Context, tenant, id string, events []session.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms := m.get(tenant, id)
	if ms == nil {
		return session.ErrNotFound
	}
	if ms.s.Status != session.Running {
		return session.ErrEnded
	}
	next, s := ms.seq, ms.s
	var add []session.Event
	for _, e := range events {
		if e.Seq <= ms.seq {
			continue // stored by an earlier attempt
		}
		if e.Seq != next+1 {
			return session.ErrGap
		}
		next = e.Seq
		e.Session, e.Body = id, bytes.Clone(e.Body)
		add = append(add, e)
		s.Updated = e.Time
		if !s.Expires.IsZero() && e.Expires.After(s.Expires) {
			s.Expires = e.Expires
		}
	}
	ms.events = append(ms.events, add...)
	ms.seq, ms.s = next, s
	return nil
}

func (m *Store) Read(_ context.Context, tenant, id string) (session.Session, []session.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms := m.get(tenant, id)
	if ms == nil {
		return session.Session{}, nil, session.ErrNotFound
	}
	now := m.now()
	var out []session.Event
	for _, e := range ms.events {
		if !expired(e.Expires, now) {
			e.Body = bytes.Clone(e.Body)
			out = append(out, e)
		}
	}
	return ms.s, out, nil
}

func (m *Store) List(_ context.Context, tenant, agent string, limit int) ([]session.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []session.Session
	for id, ms := range m.sessions {
		if ms.s.Agent == agent && m.get(tenant, id) != nil {
			out = append(out, ms.s)
		}
	}
	slices.SortFunc(out, func(a, b session.Session) int { return cmp.Or(b.Updated.Compare(a.Updated), cmp.Compare(a.ID, b.ID)) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Store) End(_ context.Context, tenant, id string, status session.Status) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms := m.get(tenant, id)
	if ms == nil {
		return session.ErrNotFound
	}
	if ms.s.Status != session.Running {
		return session.ErrEnded
	}
	ms.s.Status = status
	ms.events = slices.DeleteFunc(ms.events, func(e session.Event) bool { return e.UntilEnd })
	return nil
}

func (m *Store) Delete(_ context.Context, tenant, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.get(tenant, id) == nil {
		return session.ErrNotFound
	}
	delete(m.sessions, id)
	return nil
}

func (m *Store) DeleteAgent(_ context.Context, tenant, agent string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, ms := range m.sessions {
		if ms.s.Tenant == tenant && ms.s.Agent == agent {
			delete(m.sessions, id)
		}
	}
	return nil
}

func (m *Store) Purge(_ context.Context, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, ms := range m.sessions {
		if expired(ms.s.Expires, now) {
			n += len(ms.events)
			delete(m.sessions, id)
			continue
		}
		before := len(ms.events)
		ms.events = slices.DeleteFunc(ms.events, func(e session.Event) bool { return expired(e.Expires, now) })
		n += before - len(ms.events)
	}
	return n, nil
}

func (m *Store) ListNotes(_ context.Context, tenant, agent string) ([]session.Note, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []session.Note
	for k, n := range m.notes {
		if k[0] == tenant && k[1] == agent {
			out = append(out, n)
		}
	}
	slices.SortFunc(out, func(a, b session.Note) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

func (m *Store) PutNote(_ context.Context, n session.Note) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notes[[3]string{n.Tenant, n.Agent, n.Name}] = n
	return nil
}

func (m *Store) DeleteNote(_ context.Context, tenant, agent, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.notes, [3]string{tenant, agent, name})
	return nil
}

func (m *Store) DeleteNotes(_ context.Context, tenant, agent string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.notes {
		if k[0] == tenant && k[1] == agent {
			delete(m.notes, k)
		}
	}
	return nil
}
