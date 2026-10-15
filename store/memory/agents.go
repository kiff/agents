package memory

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"sync"

	"github.com/kiff/agents/registry"
)

// Agents is a registry.Agents held in memory.
type Agents struct {
	mu   sync.Mutex
	defs map[[2]string]registry.Definition // by tenant, id
}

// NewAgents returns an empty registry.
func NewAgents() *Agents { return &Agents{defs: map[[2]string]registry.Definition{}} }

var _ registry.Agents = (*Agents)(nil)

// clone copies d's keep rules, the only part a caller could change in place.
func clone(d registry.Definition) registry.Definition {
	d.Keep = maps.Clone(d.Keep)
	return d
}

func (a *Agents) Put(_ context.Context, d registry.Definition) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.defs[[2]string{d.Tenant, d.ID}] = clone(d)
	return nil
}

func (a *Agents) Get(_ context.Context, tenant, id string) (registry.Definition, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	d, ok := a.defs[[2]string{tenant, id}]
	if !ok {
		return registry.Definition{}, registry.ErrNotFound
	}
	return clone(d), nil
}

func (a *Agents) List(_ context.Context, tenant string) ([]registry.Definition, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []registry.Definition
	for k, d := range a.defs {
		if k[0] == tenant {
			out = append(out, clone(d))
		}
	}
	slices.SortFunc(out, func(x, y registry.Definition) int { return cmp.Compare(x.ID, y.ID) })
	return out, nil
}

func (a *Agents) Delete(_ context.Context, tenant, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.defs, [2]string{tenant, id})
	return nil
}
