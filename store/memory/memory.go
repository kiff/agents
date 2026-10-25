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
	notes    map[[4]string]session.Note // by tenant, agent, scope, name
}

type memSession struct {
	s      session.Session
	events []session.Event
	runs   map[string]*session.RunState // every run begun, ended ones included: the last Seq stored, even if its event is gone since
}

// New returns an empty store on the system clock.
func New() *Store { return NewAt(time.Now) }

// NewAt returns an empty store whose clock decides what has expired.
func NewAt(now func() time.Time) *Store {
	return &Store{now: now, sessions: map[string]*memSession{}, notes: map[[4]string]session.Note{}}
}

var (
	_ session.Store = (*Store)(nil)
	_ session.Notes = (*Store)(nil)
)

func expired(at, now time.Time) bool { return !at.IsZero() && at.Before(now) }

func (m *Store) Create(_ context.Context, s session.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.Held = nil
	m.sessions[s.ID] = &memSession{s: s, runs: map[string]*session.RunState{}}
	return nil
}

// get returns the tenant's live session, or nil: another tenant's, or an
// expired one, is not found.
func (m *Store) get(tenant, id string) *memSession {
	ms := m.sessions[id]
	if ms == nil || ms.s.Tenant != tenant || ms.s.Expired(m.now()) {
		return nil
	}
	return ms
}

// open returns an open run's state, or nil.
func (ms *memSession) open(id string) *session.RunState {
	if r := ms.runs[id]; r != nil && r.Open {
		return r
	}
	return nil
}

func (m *Store) Begin(_ context.Context, tenant, id, run string, created time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms := m.get(tenant, id)
	switch {
	case ms == nil:
		return session.ErrNotFound
	case ms.runs[run] != nil && ms.runs[run].Open:
		return nil
	case ms.runs[run] != nil:
		return session.ErrEnded
	case m.now().Sub(created) > session.MaxStartDelay:
		return session.ErrLate
	}
	ms.runs[run] = &session.RunState{Open: true}
	return nil
}

func (m *Store) Append(_ context.Context, tenant, id, run string, events []session.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms := m.get(tenant, id)
	if ms == nil {
		return session.ErrNotFound
	}
	r := ms.open(run)
	if r == nil {
		return session.ErrEnded
	}
	next, s := r.Seq, ms.s
	var add []session.Event
	for _, e := range events {
		if e.Seq <= r.Seq {
			continue // stored by an earlier attempt
		}
		if e.Seq != next+1 {
			return session.ErrGap
		}
		next = e.Seq
		e.Session, e.Run, e.Body = id, run, bytes.Clone(e.Body)
		add = append(add, e)
		s.Updated = e.Time
		if !s.Expires.IsZero() && e.Expires.After(s.Expires) {
			s.Expires = e.Expires
		}
	}
	ms.events = append(ms.events, add...)
	r.Seq, ms.s = next, s
	return nil
}

// live returns copies of the events that pass keep, unexpired as of now, in
// SortEvents order.
func live(events []session.Event, now time.Time, keep func(session.Event) bool) []session.Event {
	var out []session.Event
	for _, e := range events {
		if !expired(e.Expires, now) && keep(e) {
			e.Body = bytes.Clone(e.Body)
			out = append(out, e)
		}
	}
	session.SortEvents(out)
	return out
}

func (m *Store) Read(_ context.Context, tenant, id string) (session.Session, []session.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms := m.get(tenant, id)
	if ms == nil {
		return session.Session{}, nil, session.ErrNotFound
	}
	s := ms.s
	s.Held = slices.Clone(s.Held)
	return s, live(ms.events, m.now(), func(session.Event) bool { return true }), nil
}

func (m *Store) ReadRun(_ context.Context, tenant, id, run string) (session.RunState, []session.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms := m.get(tenant, id)
	if ms == nil {
		return session.RunState{}, nil, session.ErrNotFound
	}
	var state session.RunState
	if r := ms.runs[run]; r != nil {
		state = *r
	}
	return state, live(ms.events, m.now(), func(e session.Event) bool { return e.Run == run }), nil
}

func (m *Store) State(_ context.Context, tenant, id, run string) (session.RunState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms := m.get(tenant, id)
	if ms == nil {
		return session.RunState{}, session.ErrNotFound
	}
	if r := ms.runs[run]; r != nil {
		return *r, nil
	}
	return session.RunState{}, nil
}

func (m *Store) List(_ context.Context, tenant, agent string, limit int) ([]session.Session, error) {
	return m.list(tenant, agent, func(session.Session) bool { return true }, limit)
}

func (m *Store) ListOwned(_ context.Context, tenant, agent, owner string, limit int) ([]session.Session, error) {
	return m.list(tenant, agent, func(s session.Session) bool { return s.Owner == "" || s.Owner == owner }, limit)
}

func (m *Store) list(tenant, agent string, keep func(session.Session) bool, limit int) ([]session.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []session.Session
	for id, ms := range m.sessions {
		if ms.s.Agent == agent && keep(ms.s) && m.get(tenant, id) != nil {
			s := ms.s
			s.Held = slices.Clone(s.Held)
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b session.Session) int { return cmp.Or(b.Updated.Compare(a.Updated), cmp.Compare(a.ID, b.ID)) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Store) SetArchived(_ context.Context, tenant, id string, archived bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms := m.get(tenant, id)
	if ms == nil {
		return session.ErrNotFound
	}
	ms.s.Archived = archived
	return nil
}

func (m *Store) Hold(_ context.Context, tenant, id, run string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms := m.get(tenant, id)
	if ms == nil {
		return session.ErrNotFound
	}
	r := ms.open(run)
	if r == nil {
		return session.ErrEnded
	}
	if !r.Held {
		r.Held = true
		ms.s.Held = append(ms.s.Held, run)
		slices.Sort(ms.s.Held)
	}
	return nil
}

func (m *Store) EndRun(_ context.Context, tenant, id, run string, expires time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms := m.get(tenant, id)
	if ms == nil {
		return session.ErrNotFound
	}
	r := ms.open(run)
	if r == nil {
		return session.ErrEnded
	}
	r.Open, r.Ended, r.Held = false, true, false
	ms.s.Held = slices.DeleteFunc(ms.s.Held, func(h string) bool { return h == run })
	if len(ms.s.Held) == 0 {
		ms.s.Held = nil
	}
	if !ms.s.Expires.IsZero() && expires.After(ms.s.Expires) {
		ms.s.Expires = expires
	}
	ms.events = slices.DeleteFunc(ms.events, func(e session.Event) bool { return e.Run == run && e.UntilEnd })
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
		if ms.s.Expired(now) {
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
	slices.SortFunc(out, func(a, b session.Note) int { return cmp.Or(cmp.Compare(a.Scope, b.Scope), cmp.Compare(a.Name, b.Name)) })
	return out, nil
}

func (m *Store) PutNote(_ context.Context, n session.Note) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notes[[4]string{n.Tenant, n.Agent, n.Scope, n.Name}] = n
	return nil
}

func (m *Store) DeleteNote(_ context.Context, tenant, agent, scope, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.notes, [4]string{tenant, agent, scope, name})
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
