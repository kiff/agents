package memory

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/kiff/agents/files"
)

// Files is a files.Store held in memory.
type Files struct {
	mu sync.Mutex
	f  map[[2]string]files.File // by tenant, id
}

// NewFiles returns an empty delivered-files store.
func NewFiles() *Files { return &Files{f: map[[2]string]files.File{}} }

var (
	_ files.Store = (*Files)(nil)
	_ files.Blobs = (*Blobs)(nil)
)

// Create stores f.
func (s *Files) Create(_ context.Context, f files.File) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.f[[2]string{f.Tenant, f.ID}]; ok {
		return fmt.Errorf("memory: file %s exists", f.ID)
	}
	s.f[[2]string{f.Tenant, f.ID}] = f
	return nil
}

// Get reads one.
func (s *Files) Get(_ context.Context, tenant, id string) (files.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.f[[2]string{tenant, id}]
	if !ok {
		return files.File{}, files.ErrNotFound
	}
	return f, nil
}

// List lists the tenant's.
func (s *Files) List(_ context.Context, tenant string) ([]files.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []files.File
	for k, f := range s.f {
		if k[0] == tenant {
			out = append(out, f)
		}
	}
	return out, nil
}

// Due lists what is due by now, oldest first.
func (s *Files) Due(_ context.Context, now time.Time, limit int) ([]files.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []files.File
	for _, f := range s.f {
		if !f.Expires.After(now) {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(x, y files.File) int { return x.Expires.Compare(y.Expires) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Delete removes one.
func (s *Files) Delete(_ context.Context, tenant, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.f, [2]string{tenant, id})
	return nil
}
