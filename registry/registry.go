// Package registry holds agents as stored definitions, and builds an agent
// from its definition for each request, for its tenant.
//
// A definition never holds the agent's KIFF key: only a reference to it,
// resolved within the tenant when the agent is built. Building also refuses a
// gateway the server doesn't know, so no definition can send its key, or its
// write calls, anywhere but KIFF's gateway.
package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/skill"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/show"
)

// MaxSize bounds a definition as stored (Encode), in bytes, so it always fits
// one item, with room for its keys: DynamoDB's limit is 400 KB.
const MaxSize = 300_000

// maxID bounds an agent's ID, and maxTenant a tenant.
const (
	maxID     = 128
	maxTenant = 256
)

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
	// Effort is how hard the model works ("low", "medium", "high", "xhigh",
	// "max"); empty is the provider's default (medium on Opus 5.5).
	Effort string `json:"effort,omitempty"`

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
	// HistoryTokens bounds what those runs' messages may take, in tokens
	// (estimated). In a chat, the runs that no longer fit, by count or by
	// size, are summarised by the model instead of dropped, and the summary
	// comes first (session.Summary). 0: no bound by size, and no summary.
	HistoryTokens int `json:"history_tokens,omitempty"`
	// Workspace gives each of the agent's runs a workspace: files and
	// commands, with no credentials and no way out (#86). Off unless set.
	Workspace bool `json:"workspace,omitempty"`
	// Show gives the agent the show tool: UI components in the owner's chat
	// (kiff/agents#91). Off unless set.
	Show bool `json:"show,omitempty"`
	// Drafts gives the agent the kiff_draft tool: submit a taf's KIFF draft,
	// whose claim secret the model never sees (kiff/agents#91). Off unless
	// set; the server must offer drafts.
	Drafts bool `json:"drafts,omitempty"`
	// Planque is the planque the agent asks for vendor access through
	// (kiff/agents#91), e.g. https://planque.kiff.dev. Off unless set; the
	// server must allow it, as it does gateways.
	Planque string `json:"planque,omitempty"`
	// Skills are the tenant's skills the agent may read when it needs them
	// (#101), each a stored version: the context carries their names and
	// descriptions, and the skill tool their text.
	Skills []skill.Ref `json:"skills,omitempty"`

	// Version is the number of the stored version this is (kiff/tafs#107),
	// set by the store: the live one on Get, 0 for an agent with no versions
	// yet. It is not part of what Digest covers.
	Version int `json:"version,omitempty"`
}

// The notes a store writes itself, in the words the owner reads in the
// version list: the definition an agent had before it had versions, and a
// Put on a versioned agent (an upgrade replaces its rules as a whole).
const (
	NoteFirst = "As it was set up"
	NotePut   = "Its rules were updated"
)

// Version is one stored version of an agent (kiff/tafs#107): its
// definition is kept with it, and any one may go live, forward or back.
type Version struct {
	N int `json:"n"`
	// Note is what changed, in plain words.
	Note string `json:"note,omitempty"`
	// By is who made it: the owner's subject, or "setup".
	By      string    `json:"by,omitempty"`
	Created time.Time `json:"created"`
	// Digest is Digest(the version's definition).
	Digest string `json:"digest"`
	Live   bool   `json:"live"`
	// Revisions are the KIFF Card revisions this version needs live; empty
	// is the agent's current Cards.
	Revisions []string `json:"revisions,omitempty"`
	// Draft is the KIFF draft that proposes those revisions while it isn't
	// confirmed: a trial's writes are decided against its Cards.
	Draft string `json:"draft,omitempty"`
	// Cards are the KIFF Cards this version ran under, each at the revision
	// it had when the version went live: what going back to it restores.
	Cards []CardRevision `json:"cards,omitempty"`
}

// CardRevision is one KIFF Card at one revision (kiff/tafs#107).
type CardRevision struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

// Switching marks an agent on its way to version N (kiff/tafs#107): the
// version's Card revision waits for its owner's confirmation in KIFF, and
// until the switch is done or dropped no run of the agent starts or goes on,
// so nothing runs with one version's definition under another's Cards.
// Trials still run: their writes are never sent.
type Switching struct {
	N int `json:"n"`
	// Draft is the KIFF draft whose confirmation the switch waits for.
	Draft string    `json:"draft,omitempty"`
	At    time.Time `json:"at"`
}

// ErrSwitching means the agent is switching versions: no run of it starts
// or goes on, and its definition isn't replaced, until the switch is done or
// dropped.
var ErrSwitching = errors.New("registry: this agent is switching versions; nothing of it runs until the switch is done")

// ErrNoSwitch means the agent isn't switching to that version, so there is
// no switch whose draft to replace.
var ErrNoSwitch = errors.New("registry: this agent isn't switching to that version")

// ErrSwitchChanged means the switch kept changing while its draft was being
// replaced: read it again, then replace again.
var ErrSwitchChanged = errors.New("registry: the switch changed meanwhile")

// Digest is a definition's fingerprint, its version number aside: the same
// definition stored twice has the same digest.
func Digest(d Definition) (string, error) {
	d.Version = 0
	b, err := Encode(d)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ErrNoVersion is a version the agent doesn't have.
var ErrNoVersion = errors.New("registry: no such version")

// Check refuses a definition that can't be stored or run safely.
func (d Definition) Check() error {
	switch {
	case d.Tenant == "" || len(d.Tenant) > maxTenant:
		return fmt.Errorf("registry: a tenant is 1 to %d bytes", maxTenant)
	case !validID(d.ID):
		return fmt.Errorf("registry: an id is 1 to %d letters, digits, '-', '_' or '.'", maxID)
	case d.Model == "":
		return errors.New("registry: a model is required")
	case d.Price.Input <= 0 || d.Price.Output <= 0 || d.Price.CacheRead < 0 || d.Price.CacheWrite < 0 || d.Price.CacheWrite1h < 0:
		return errors.New("registry: a price needs positive input and output, and no negative cache price")
	case d.MaxTokens <= 0:
		return errors.New("registry: max tokens must be positive")
	case d.Effort != "" && !slices.Contains(model.Efforts, d.Effort):
		return fmt.Errorf("registry: effort is empty or one of %s", strings.Join(model.Efforts, ", "))
	case d.KeyRef == "":
		return errors.New("registry: a reference to the agent's KIFF key is required")
	case d.MaxCost <= 0:
		return errors.New("registry: a run needs a cost cap")
	case d.MaxTurns < 0 || d.History < 0 || d.HistoryTokens < 0:
		return errors.New("registry: max turns and history are never negative")
	}
	if u, err := url.Parse(d.Gateway); err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("registry: the gateway is an https URL")
	}
	if d.Planque != "" {
		if u, err := url.Parse(d.Planque); err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" || u.RawQuery != "" {
			return errors.New("registry: planque is an https URL with no path")
		}
	}
	if err := d.Keep.Check(); err != nil {
		return fmt.Errorf("registry: %w", err)
	}
	if err := skill.CheckRefs(d.Skills); err != nil {
		return fmt.Errorf("registry: %w", err)
	}
	// Bound what is stored, not the raw text: encoding can make a string
	// several times longer (a control character becomes six bytes).
	b, err := Encode(d)
	if err != nil {
		return err
	}
	if len(b) > MaxSize {
		return fmt.Errorf("registry: the definition is %d bytes stored, more than %d; shorten its instructions", len(b), MaxSize)
	}
	return nil
}

// Encode is a definition as stores keep it: JSON, without escaping '<', '>'
// and '&', which instructions are full of. Check bounds its size.
func Encode(d Definition) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(d); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
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

// ErrNoSkills means a definition names skills and the builder has no store
// to read them from.
var ErrNoSkills = errors.New("registry: the agent names skills and none can be read here")

// LoadSkills reads the packages a definition names, in its order. A version
// the tenant doesn't have is an error naming it: a definition is stored only
// with skills that exist (the server checks), and a version never changes.
func LoadSkills(ctx context.Context, store skill.Store, tenant string, refs []skill.Ref) ([]skill.Package, error) {
	if store == nil {
		return nil, ErrNoSkills
	}
	ps := make([]skill.Package, 0, len(refs))
	for _, r := range refs {
		p, err := store.Get(ctx, tenant, r.Name, r.Version)
		if err != nil {
			return nil, fmt.Errorf("registry: skill %s: %w", r, err)
		}
		ps = append(ps, p)
	}
	return ps, nil
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
	// Delete removes the tenant's definition, its versions and its switching
	// mark. Deleting one
	// that isn't there is not an error, so a retry is safe.
	Delete(ctx context.Context, tenant, id string) error

	// PutVersion stores d as the agent's next version, with v's note, author,
	// revisions and draft, and returns its number. It doesn't change which
	// version is live. An agent stored before versions gets its live
	// definition kept as version 1 first. ErrNotFound for an agent not stored.
	PutVersion(ctx context.Context, d Definition, v Version) (int, error)
	// Versions lists the agent's versions, oldest first, the live one marked.
	Versions(ctx context.Context, tenant, id string) ([]Version, error)
	// GetVersion returns version n's definition (its Version set to n), or
	// ErrNoVersion.
	GetVersion(ctx context.Context, tenant, id string, n int) (Definition, error)
	// Switch makes version n live: Get returns it from then on. Forward or
	// back, to any version the agent has; ErrNoVersion otherwise.
	Switch(ctx context.Context, tenant, id string, n int) error

	// SetCards records the KIFF Cards version n runs under, replacing what
	// was recorded; ErrNoVersion for a version the agent doesn't have.
	SetCards(ctx context.Context, tenant, id string, n int, cards []CardRevision) error
	// BeginSwitch marks the agent as switching to sw.N and returns the mark
	// as stored. Again for the same N keeps the first mark; for another N,
	// ErrSwitching. ErrNotFound for an agent not stored.
	BeginSwitch(ctx context.Context, tenant, id string, sw Switching) (Switching, error)
	// SwitchDraft replaces the KIFF draft a switch to n waits for, keeping
	// the mark, so nothing of the agent runs in between (kiff/tafs#107: the
	// version's Cards drafted again). ErrNoSwitch when it isn't switching to n.
	SwitchDraft(ctx context.Context, tenant, id string, n int, draft string) (Switching, error)
	// Switching returns the agent's mark, and whether it has one.
	Switching(ctx context.Context, tenant, id string) (Switching, bool, error)
	// EndSwitch removes the agent's mark; none is not an error.
	EndSwitch(ctx context.Context, tenant, id string) error
}

// Secrets resolves the reference to an agent's KIFF key. An implementation
// resolves ref only within tenant: no reference reaches another tenant's key.
type Secrets interface {
	Key(ctx context.Context, tenant, ref string) (string, error)
}

// ErrGateway means a definition names a gateway the server doesn't allow.
var ErrGateway = errors.New("registry: the gateway is not one this server allows")

// ErrNoDrafts means a definition asks for the draft tool this build can't
// give.
var ErrNoDrafts = errors.New("registry: the agent needs the draft tool, and this build has none")

// ErrPlanque means a definition names a planque the server doesn't allow.
var ErrPlanque = errors.New("registry: the planque is not one this server allows")

// ErrNoWorkspace means a definition asks for a workspace this build can't
// give: the server has none, or the agent is built for no run.
var ErrNoWorkspace = errors.New("registry: the agent needs a workspace, and this build has none")

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
	// Workspace, when set, returns the workspace tool for one run of an
	// agent whose definition asks for one.
	Workspace func(tenant, run string) tool.Tool
	// Drafts, when set, returns the draft tool for an agent whose KIFF key
	// is key.
	Drafts func(tenant, key string) tool.Tool
	// Planques are the planque URLs a definition may name, exactly.
	Planques []string
	// Planque returns the planque tool for an agent whose KIFF key is key.
	Planque func(base, tenant, key string) tool.Tool
	// Skills holds the tenants' skill packages, for definitions that name
	// some (#101). Without it, such a definition doesn't build.
	Skills skill.Store
}

// Build returns the tenant's agent, ready to run, and the definition it was
// built from (its keep rules and history size are the caller's to apply).
// Its gateway tools are listed now, so a run sees what its Cards reach today.
// An agent whose definition asks for a workspace is built only for a run,
// with BuildRun.
func (b *Builder) Build(ctx context.Context, tenant, id string) (agent.Agent, Definition, error) {
	return b.BuildRun(ctx, tenant, id, "")
}

// BuildRun is Build for one run of the agent: one whose definition asks for
// a workspace gets that run's.
func (b *Builder) BuildRun(ctx context.Context, tenant, id, run string) (agent.Agent, Definition, error) {
	return b.BuildVersion(ctx, tenant, id, run, 0)
}

// BuildVersion is BuildRun for version n of the agent, not the live one: a
// trial of a version that isn't live yet (kiff/tafs#107). 0 is the live one.
func (b *Builder) BuildVersion(ctx context.Context, tenant, id, run string, n int) (agent.Agent, Definition, error) {
	var d Definition
	var err error
	if n > 0 {
		d, err = b.Agents.GetVersion(ctx, tenant, id, n)
	} else {
		d, err = b.Agents.Get(ctx, tenant, id)
	}
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
	// Like the gateway, a planque is checked before the key is looked up.
	if d.Planque != "" && (b.Planque == nil || !slices.Contains(b.Planques, d.Planque)) {
		return agent.Agent{}, d, ErrPlanque
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
	if d.Show {
		tools = append(tools, show.New())
	}
	if d.Drafts {
		if b.Drafts == nil {
			return agent.Agent{}, d, ErrNoDrafts
		}
		tools = append(tools, b.Drafts(tenant, key))
	}
	if d.Planque != "" {
		tools = append(tools, b.Planque(d.Planque, tenant, key))
	}
	if d.Workspace {
		if b.Workspace == nil || run == "" {
			return agent.Agent{}, d, ErrNoWorkspace
		}
		tools = append(tools, b.Workspace(tenant, run))
	}
	system := d.Instructions
	if len(d.Skills) > 0 {
		ps, err := LoadSkills(ctx, b.Skills, tenant, d.Skills)
		if err != nil {
			return agent.Agent{}, d, err
		}
		system += "\n\n" + skill.Catalog(ps)
		tools = append(tools, skill.NewTool(ps))
	}
	a := agent.Agent{Tenant: tenant, ID: id, Model: b.Model, ModelID: d.Model, Price: d.Price,
		MaxTokens: d.MaxTokens, Effort: d.Effort, System: system, Tools: tools, MaxCost: d.MaxCost, MaxTurns: d.MaxTurns}
	if err := a.Check(); err != nil {
		return agent.Agent{}, d, err
	}
	return a, d, nil
}
