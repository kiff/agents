// Package registry holds agents as stored definitions, and builds an agent
// from its definition for each request, for its tenant.
//
// A definition never holds the agent's KIFF key: only a reference to it,
// resolved within the tenant when the agent is built. Building also refuses a
// gateway the server doesn't know, so no definition can send its key, or its
// write calls, anywhere but KIFF's gateway.
package registry

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool"
)

// MaxInstructions bounds a definition's instructions, in bytes, so a
// definition always fits one stored item.
const MaxInstructions = 200_000

// maxID bounds an agent's ID.
const maxID = 128

// Definition is a stored agent: what it is told, which model at which price,
// where its tools are, what is kept of its runs, and what a run may spend.
type Definition struct {
	Tenant string `json:"tenant"`
	ID     string `json:"id"`

	// Instructions are the agent's system prompt, KIFF's skill included.
	Instructions string `json:"instructions"`

	// Model is the provider's model id, and Price what it charges.
	Model     string      `json:"model"`
	Price     model.Price `json:"price"`
	MaxTokens int64       `json:"max_tokens"`

	// Gateway is the URL of KIFF's gateway the agent's tools come from.
	Gateway string `json:"gateway"`
	// KeyRef names the agent's KIFF key in the tenant's secrets. The key
	// itself is never stored here.
	KeyRef string `json:"key_ref"`

	// Keep is the keep rule for every kind of event its runs record.
	Keep session.Policy `json:"keep"`

	// MaxCost is the most a run may cost, in millionths of a US dollar.
	MaxCost int64 `json:"max_cost_micro_usd"`
	// MaxTurns bounds the model turns of a run; 0 means agent.DefaultMaxTurns.
	MaxTurns int `json:"max_turns,omitempty"`
	// History is how many of the session's finished runs the model gets
	// before a new one.
	History int `json:"history"`
}

// Check refuses a definition that can't be stored or run safely.
func (d Definition) Check() error {
	switch {
	case d.Tenant == "":
		return errors.New("registry: a tenant is required")
	case !validID(d.ID):
		return fmt.Errorf("registry: an id is 1 to %d letters, digits, '-', '_' or '.'", maxID)
	case len(d.Instructions) > MaxInstructions:
		return fmt.Errorf("registry: instructions are longer than %d bytes", MaxInstructions)
	case d.Model == "":
		return errors.New("registry: a model is required")
	case d.Price.Input <= 0 || d.Price.Output <= 0 || d.Price.CacheRead < 0 || d.Price.CacheWrite < 0:
		return errors.New("registry: a price needs positive input and output, and no negative cache price")
	case d.MaxTokens <= 0:
		return errors.New("registry: max tokens must be positive")
	case d.KeyRef == "":
		return errors.New("registry: a reference to the agent's KIFF key is required")
	case d.MaxCost <= 0:
		return errors.New("registry: a run needs a cost cap")
	case d.MaxTurns < 0 || d.History < 0:
		return errors.New("registry: max turns and history are never negative")
	}
	if u, err := url.Parse(d.Gateway); err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("registry: the gateway is an https URL")
	}
	if err := d.Keep.Check(); err != nil {
		return fmt.Errorf("registry: %w", err)
	}
	return nil
}

func validID(id string) bool {
	if id == "" || len(id) > maxID {
		return false
	}
	for _, c := range []byte(id) {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// ErrNotFound means the tenant has no agent with that ID.
var ErrNotFound = errors.New("registry: no such agent")

// Agents stores definitions. Every call is scoped by tenant: another tenant's
// agent is never found, listed or deleted.
type Agents interface {
	// Put stores d, replacing the tenant's definition with its ID.
	Put(ctx context.Context, d Definition) error
	// Get returns the tenant's definition, or ErrNotFound.
	Get(ctx context.Context, tenant, id string) (Definition, error)
	// List returns the tenant's definitions, by ID.
	List(ctx context.Context, tenant string) ([]Definition, error)
	// Delete removes the tenant's definition. Deleting one that isn't there
	// is not an error, so a retry is safe.
	Delete(ctx context.Context, tenant, id string) error
}

// Secrets resolves the reference to an agent's KIFF key. An implementation
// resolves ref only within tenant: no reference reaches another tenant's key.
type Secrets interface {
	Key(ctx context.Context, tenant, ref string) (string, error)
}

// ErrGateway means a definition names a gateway the server doesn't allow.
var ErrGateway = errors.New("registry: the gateway is not one this server allows")

// Builder builds agents from their definitions, once per request.
type Builder struct {
	Agents  Agents
	Secrets Secrets
	Model   model.Model
	// Gateways are the gateway URLs a definition may name, exactly.
	Gateways []string
	// Connect lists the tools a gateway offers the agent whose KIFF key is
	// key.
	Connect func(ctx context.Context, gateway, key string) ([]tool.Tool, error)
	// Own, when set, returns the tools an agent has besides its gateway's,
	// such as its memory.
	Own func(tenant, agent string) []tool.Tool
}

// Build returns the tenant's agent, ready to run, and the definition it was
// built from (its keep rules and history size are the caller's to apply).
// Its gateway tools are listed now, so a run sees what its Cards reach today.
func (b *Builder) Build(ctx context.Context, tenant, id string) (agent.Agent, Definition, error) {
	d, err := b.Agents.Get(ctx, tenant, id)
	if err != nil {
		return agent.Agent{}, Definition{}, err
	}
	if d.Tenant != tenant || d.ID != id {
		return agent.Agent{}, Definition{}, fmt.Errorf("registry: the store returned agent %q of another tenant or id", d.ID)
	}
	if err := d.Check(); err != nil {
		return agent.Agent{}, d, err
	}
	if !slices.Contains(b.Gateways, d.Gateway) {
		return agent.Agent{}, d, ErrGateway
	}
	key, err := b.Secrets.Key(ctx, tenant, d.KeyRef)
	if err != nil {
		return agent.Agent{}, d, fmt.Errorf("registry: the agent's KIFF key: %w", err)
	}
	tools, err := b.Connect(ctx, d.Gateway, key)
	if err != nil {
		return agent.Agent{}, d, err
	}
	if b.Own != nil {
		tools = append(tools, b.Own(tenant, id)...)
	}
	a := agent.Agent{Tenant: tenant, ID: id, Model: b.Model, ModelID: d.Model, Price: d.Price,
		MaxTokens: d.MaxTokens, System: d.Instructions, Tools: tools, MaxCost: d.MaxCost, MaxTurns: d.MaxTurns}
	if err := a.Check(); err != nil {
		return agent.Agent{}, d, err
	}
	return a, d, nil
}
