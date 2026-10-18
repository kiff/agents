package memory

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"sync"

	"github.com/kiff/agents/skill"
)

// Skills is a skill.Store held in memory.
type Skills struct {
	mu sync.Mutex
	ps map[[3]string]skill.Package // by tenant, name, version
}

// NewSkills returns an empty skill store.
func NewSkills() *Skills { return &Skills{ps: map[[3]string]skill.Package{}} }

var _ skill.Store = (*Skills)(nil)

func (s *Skills) Put(_ context.Context, p skill.Package) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := [3]string{p.Tenant, p.Name, p.Version}
	if old, ok := s.ps[k]; ok {
		if !old.Same(p) {
			return skill.ErrExists
		}
		return nil
	}
	p.Files = maps.Clone(p.Files)
	s.ps[k] = p
	return nil
}

func (s *Skills) Get(_ context.Context, tenant, name, version string) (skill.Package, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.ps[[3]string{tenant, name, version}]
	if !ok {
		return skill.Package{}, skill.ErrNotFound
	}
	p.Files = maps.Clone(p.Files)
	return p, nil
}

func (s *Skills) List(_ context.Context, tenant string) ([]skill.Package, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []skill.Package
	for k, p := range s.ps {
		if k[0] == tenant {
			p.Body, p.Files = "", nil
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(x, y skill.Package) int {
		return cmp.Or(cmp.Compare(x.Name, y.Name), cmp.Compare(x.Version, y.Version))
	})
	return out, nil
}
