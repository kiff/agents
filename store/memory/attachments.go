package memory

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kiff/agents/attach"
)

// Attachments is an attach.Store held in memory.
type Attachments struct {
	mu sync.Mutex
	a  map[[2]string]attach.Attachment // by tenant, id
}

// NewAttachments returns an empty attachments store.
func NewAttachments() *Attachments {
	return &Attachments{a: map[[2]string]attach.Attachment{}}
}

var _ attach.Store = (*Attachments)(nil)

// Create stores a.
func (s *Attachments) Create(_ context.Context, a attach.Attachment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.a[[2]string{a.Tenant, a.ID}]; ok {
		return fmt.Errorf("memory: attachment %s exists", a.ID)
	}
	s.a[[2]string{a.Tenant, a.ID}] = a
	return nil
}

// Get reads one.
func (s *Attachments) Get(_ context.Context, tenant, id string) (attach.Attachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.a[[2]string{tenant, id}]
	if !ok {
		return attach.Attachment{}, attach.ErrNotFound
	}
	return a, nil
}

// Check marks it checked.
func (s *Attachments) Check(_ context.Context, tenant, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.a[[2]string{tenant, id}]
	if !ok {
		return attach.ErrNotFound
	}
	a.Checked = true
	s.a[[2]string{tenant, id}] = a
	return nil
}

// Bind ties an unbound one to its message.
func (s *Attachments) Bind(_ context.Context, tenant, id, session, run string, at, expires time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.a[[2]string{tenant, id}]
	switch {
	case !ok:
		return attach.ErrNotFound
	case a.Session != "":
		return attach.ErrBound
	}
	a.Session, a.Run, a.Bound, a.Expires = session, run, at, expires
	s.a[[2]string{tenant, id}] = a
	return nil
}

func (s *Attachments) list(keep func(attach.Attachment) bool) []attach.Attachment {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []attach.Attachment
	for _, a := range s.a {
		if keep(a) {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(x, y attach.Attachment) int { return x.Created.Compare(y.Created) })
	return out
}

// Session lists a session's.
func (s *Attachments) Session(_ context.Context, tenant, session string) ([]attach.Attachment, error) {
	return s.list(func(a attach.Attachment) bool { return a.Tenant == tenant && a.Session == session }), nil
}

// Agent lists an agent's.
func (s *Attachments) Agent(_ context.Context, tenant, agent string) ([]attach.Attachment, error) {
	return s.list(func(a attach.Attachment) bool { return a.Tenant == tenant && a.Agent == agent }), nil
}

// Due lists what is due by now, oldest due first.
func (s *Attachments) Due(_ context.Context, now time.Time, limit int) ([]attach.Attachment, error) {
	out := s.list(func(a attach.Attachment) bool { d := a.Due(); return !d.IsZero() && !d.After(now) })
	slices.SortFunc(out, func(x, y attach.Attachment) int { return x.Due().Compare(y.Due()) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Delete removes one.
func (s *Attachments) Delete(_ context.Context, tenant, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.a, [2]string{tenant, id})
	return nil
}

// Blobs is an attach.Blobs held in memory. Its upload and download URLs are
// "memory:" plus the key: a test puts the bytes with Put.
type Blobs struct {
	mu sync.Mutex
	b  map[string][]byte
}

// NewBlobs returns an empty blob store.
func NewBlobs() *Blobs { return &Blobs{b: map[string][]byte{}} }

var _ attach.Blobs = (*Blobs)(nil)

// PresignPut is "memory:" plus key.
func (s *Blobs) PresignPut(_ context.Context, key, mediaType string, size int64, sha256 string, ttl time.Duration) (attach.Upload, error) {
	return attach.Upload{URL: "memory:" + key, Method: "PUT", Headers: map[string]string{"Content-Type": mediaType},
		Expires: time.Now().Add(ttl)}, nil
}

// PresignGet is "memory:" plus key.
func (s *Blobs) PresignGet(_ context.Context, key, _ string, _ time.Duration) (string, error) {
	return "memory:" + key, nil
}

// Get reads key.
func (s *Blobs) Get(_ context.Context, key string, max int64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.b[key]
	switch {
	case !ok:
		return nil, fmt.Errorf("memory: no blob %s", key)
	case int64(len(b)) > max:
		return nil, fmt.Errorf("memory: %s is over %d bytes", key, max)
	}
	return bytes.Clone(b), nil
}

// Put writes key.
func (s *Blobs) Put(_ context.Context, key, _ string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b[key] = bytes.Clone(data)
	return nil
}

// Delete removes keys.
func (s *Blobs) Delete(_ context.Context, keys ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		delete(s.b, k)
	}
	return nil
}

// DeletePrefix removes every key under prefix.
func (s *Blobs) DeletePrefix(_ context.Context, prefix string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.b {
		if strings.HasPrefix(k, prefix) {
			delete(s.b, k)
		}
	}
	return nil
}

// Keys lists what is stored, for tests.
func (s *Blobs) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.b {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
