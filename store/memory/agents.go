package memory

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/kiff/agents/registry"
)

// Agents is a registry.Agents held in memory.
type Agents struct {
	mu   sync.Mutex
	defs map[[2]string]registry.Definition // by tenant, id: the live one
	vers map[[2]string][]version           // by tenant, id: every version, oldest first
	sw   map[[2]string]registry.Switching  // by tenant, id: a switch under way
	halt map[[2]string]registry.Halt       // by tenant, id (registry.Tenant: the tenant's own)
	// Now stamps a version's creation; nil is time.Now.
	Now func() time.Time
}

// version is one stored version: its record and its definition.
type version struct {
	v registry.Version
	d registry.Definition
}

// NewAgents returns an empty registry.
func NewAgents() *Agents {
	return &Agents{defs: map[[2]string]registry.Definition{}, vers: map[[2]string][]version{}, sw: map[[2]string]registry.Switching{}, halt: map[[2]string]registry.Halt{}}
}

func (a *Agents) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now().UTC()
}

var _ registry.Agents = (*Agents)(nil)

// clone copies d's keep rules, the only part a caller could change in place.
func clone(d registry.Definition) registry.Definition {
	d.Keep = maps.Clone(d.Keep)
	return d
}

// Put stores d as the live definition. An agent with versions gets it as its
// next version, live at once (an upgrade is part of its history); one
// without stays unversioned.
func (a *Agents) Put(_ context.Context, d registry.Definition) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := [2]string{d.Tenant, d.ID}
	d.Version = 0
	if vs := a.vers[k]; len(vs) > 0 {
		dg, err := registry.Digest(d)
		if err != nil {
			return err
		}
		d.Version = len(vs) + 1
		a.vers[k] = append(vs, version{v: registry.Version{N: d.Version, Note: registry.NotePut, Created: a.now(), Digest: dg}, d: clone(d)})
	}
	a.defs[k] = clone(d)
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
	delete(a.vers, [2]string{tenant, id})
	delete(a.sw, [2]string{tenant, id})
	if id != registry.Tenant {
		delete(a.halt, [2]string{tenant, id})
	}
	return nil
}

func (a *Agents) PutVersion(_ context.Context, d registry.Definition, v registry.Version) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := [2]string{d.Tenant, d.ID}
	live, ok := a.defs[k]
	if !ok {
		return 0, registry.ErrNotFound
	}
	if len(a.vers[k]) == 0 { // stored before versions: what's live is version 1
		dg, err := registry.Digest(live)
		if err != nil {
			return 0, err
		}
		live.Version = 1
		a.defs[k] = live
		a.vers[k] = []version{{v: registry.Version{N: 1, Note: registry.NoteFirst, Created: a.now(), Digest: dg}, d: clone(live)}}
	}
	dg, err := registry.Digest(d)
	if err != nil {
		return 0, err
	}
	v.N, v.Digest, v.Created, v.Live = len(a.vers[k])+1, dg, a.now(), false
	d.Version = v.N
	v.Revisions = slices.Clone(v.Revisions)
	a.vers[k] = append(a.vers[k], version{v: v, d: clone(d)})
	return v.N, nil
}

func (a *Agents) Versions(_ context.Context, tenant, id string) ([]registry.Version, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := [2]string{tenant, id}
	live, ok := a.defs[k]
	if !ok {
		return nil, registry.ErrNotFound
	}
	out := make([]registry.Version, 0, len(a.vers[k]))
	for _, x := range a.vers[k] {
		v := x.v
		v.Live = v.N == live.Version
		v.Revisions = slices.Clone(v.Revisions)
		v.Cards = slices.Clone(v.Cards)
		out = append(out, v)
	}
	return out, nil
}

func (a *Agents) GetVersion(_ context.Context, tenant, id string, n int) (registry.Definition, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	vs := a.vers[[2]string{tenant, id}]
	if n < 1 || n > len(vs) {
		return registry.Definition{}, registry.ErrNoVersion
	}
	return clone(vs[n-1].d), nil
}

func (a *Agents) Switch(_ context.Context, tenant, id string, n int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := [2]string{tenant, id}
	vs := a.vers[k]
	if _, ok := a.defs[k]; !ok {
		return registry.ErrNotFound
	}
	if n < 1 || n > len(vs) {
		return registry.ErrNoVersion
	}
	a.defs[k] = clone(vs[n-1].d)
	return nil
}

func (a *Agents) BeginSwitch(_ context.Context, tenant, id string, sw registry.Switching) (registry.Switching, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := [2]string{tenant, id}
	if _, ok := a.defs[k]; !ok {
		return registry.Switching{}, registry.ErrNotFound
	}
	if was, ok := a.sw[k]; ok {
		if was.N != sw.N {
			return was, registry.ErrSwitching
		}
		return was, nil
	}
	sw.At = sw.At.UTC()
	a.sw[k] = sw
	return sw, nil
}

func (a *Agents) SwitchDraft(_ context.Context, tenant, id string, n int, draft string) (registry.Switching, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := [2]string{tenant, id}
	sw, ok := a.sw[k]
	if !ok || sw.N != n {
		return registry.Switching{}, registry.ErrNoSwitch
	}
	sw.Draft = draft
	a.sw[k] = sw
	return sw, nil
}

func (a *Agents) Switching(_ context.Context, tenant, id string) (registry.Switching, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sw, ok := a.sw[[2]string{tenant, id}]
	return sw, ok, nil
}

func (a *Agents) EndSwitch(_ context.Context, tenant, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sw, [2]string{tenant, id})
	return nil
}

func (a *Agents) SetCards(_ context.Context, tenant, id string, n int, cards []registry.CardRevision) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	vs := a.vers[[2]string{tenant, id}]
	if n < 1 || n > len(vs) {
		return registry.ErrNoVersion
	}
	vs[n-1].v.Cards = slices.Clone(cards)
	return nil
}

func (a *Agents) Halt(_ context.Context, tenant, id string, at time.Time) (registry.Halt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := [2]string{tenant, id}
	if _, ok := a.defs[k]; !ok && id != registry.Tenant {
		return registry.Halt{}, registry.ErrNotFound
	}
	h := a.halt[k]
	if !h.On {
		h = registry.Halt{On: true, At: at.UTC()}
		a.halt[k] = h
	}
	return h, nil
}

func (a *Agents) Resume(_ context.Context, tenant, id string) (registry.Halt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := [2]string{tenant, id}
	if _, ok := a.defs[k]; !ok && id != registry.Tenant {
		return registry.Halt{}, registry.ErrNotFound
	}
	h, ok := a.halt[k]
	if ok {
		h.On = false
		a.halt[k] = h
	}
	return h, nil
}

func (a *Agents) Halted(_ context.Context, tenant, id string) (registry.Halt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.halt[[2]string{tenant, id}], nil
}

// EraseTenant deletes everything the tenant keeps here, when its account is
// deleted (kiff/tafs#185).
func (a *Agents) EraseTenant(_ context.Context, tenant string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k := range a.defs {
		if k[0] == tenant {
			delete(a.defs, k)
		}
	}
	for k := range a.vers {
		if k[0] == tenant {
			delete(a.vers, k)
		}
	}
	for k := range a.sw {
		if k[0] == tenant {
			delete(a.sw, k)
		}
	}
	for k := range a.halt {
		if k[0] == tenant {
			delete(a.halt, k)
		}
	}
	return nil
}
