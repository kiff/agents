package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/registry/registrytest"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/kiff"
)

var ctx = context.Background()

func TestCheckRefuses(t *testing.T) {
	cases := map[string]func(*registry.Definition){
		"no tenant":          func(d *registry.Definition) { d.Tenant = "" },
		"no id":              func(d *registry.Definition) { d.ID = "" },
		"an id with a slash": func(d *registry.Definition) { d.ID = "a/b" },
		"a long id":          func(d *registry.Definition) { d.ID = strings.Repeat("a", 129) },
		"long instructions":  func(d *registry.Definition) { d.Instructions = strings.Repeat("x", registry.MaxSize) },
		"a long tenant":      func(d *registry.Definition) { d.Tenant = strings.Repeat("t", 257) },
		// 60,000 bytes of text, but 360,000 once encoded.
		"escapes past the limit": func(d *registry.Definition) { d.Instructions = strings.Repeat("\x01", 60_000) },
		"no model":               func(d *registry.Definition) { d.Model = "" },
		"no output price":        func(d *registry.Definition) { d.Price.Output = 0 },
		"a negative cache":       func(d *registry.Definition) { d.Price.CacheRead = -1 },
		"no max tokens":          func(d *registry.Definition) { d.MaxTokens = 0 },
		"an unknown effort":      func(d *registry.Definition) { d.Effort = "extreme" },
		"an effort's case":       func(d *registry.Definition) { d.Effort = "High" },
		"no key reference":       func(d *registry.Definition) { d.KeyRef = "" },
		"no cost cap":            func(d *registry.Definition) { d.MaxCost = 0 },
		"negative turns":         func(d *registry.Definition) { d.MaxTurns = -1 },
		"negative history":       func(d *registry.Definition) { d.History = -1 },
		"a plain http gateway":   func(d *registry.Definition) { d.Gateway = "http://gateway.test/mcp" },
		"no gateway":             func(d *registry.Definition) { d.Gateway = "" },
		"a plain http planque":   func(d *registry.Definition) { d.Planque = "http://planque.test" },
		"a planque with a path":  func(d *registry.Definition) { d.Planque = "https://planque.test/v1" },
		"a missing keep rule":    func(d *registry.Definition) { delete(d.Keep, session.Usage) },
	}
	if err := registrytest.Definition("t1", "payouts").Check(); err != nil {
		t.Fatalf("the valid definition is refused: %v", err)
	}
	for _, e := range append([]string{""}, model.Efforts...) { // none is the provider's default
		d := registrytest.Definition("t1", "payouts")
		d.Effort = e
		if err := d.Check(); err != nil {
			t.Errorf("effort %q refused: %v", e, err)
		}
	}
	for name, change := range cases {
		d := registrytest.Definition("t1", "payouts")
		change(&d)
		if err := d.Check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// stub is a tool with a name and an access.
type stub struct {
	name    string
	access  tool.Access
	gateway bool
}

func (s stub) Spec() model.ToolSpec {
	return model.ToolSpec{Name: s.name, InputSchema: json.RawMessage(`{}`)}
}
func (s stub) Access() tool.Access { return s.access }
func (s stub) Gateway() bool       { return s.gateway }
func (s stub) Call(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{}, nil
}

type model0 struct{}

func (model0) Turn(context.Context, model.Request, func(string)) (model.Reply, error) {
	return model.Reply{}, nil
}

// secrets holds keys by tenant and reference, and counts lookups.
type secrets struct {
	keys  map[[2]string]string
	calls int
}

func (s *secrets) Key(_ context.Context, tenant, ref string) (string, error) {
	s.calls++
	k, ok := s.keys[[2]string{tenant, ref}]
	if !ok {
		return "", errors.New("no such secret")
	}
	return k, nil
}

// fixture is a builder over a memory registry holding t1's agent "payouts",
// whose key is "key-t1".
type fixture struct {
	b        *registry.Builder
	agents   *memory.Agents
	secrets  *secrets
	connects []string // "gateway key" for each Connect
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{agents: memory.NewAgents(),
		secrets: &secrets{keys: map[[2]string]string{{"t1", "agents/payouts"}: "key-t1", {"t2", "agents/payouts"}: "key-t2"}}}
	if err := f.agents.Put(ctx, registrytest.Definition("t1", "payouts")); err != nil {
		t.Fatal(err)
	}
	f.b = &registry.Builder{Agents: f.agents, Secrets: f.secrets, Model: model0{},
		Gateways: []string{"https://gateway.test/mcp"},
		Connect: func(_ context.Context, gateway, key string) ([]tool.Tool, error) {
			f.connects = append(f.connects, gateway+" "+key)
			return []tool.Tool{stub{name: "stripe_refund", access: tool.Write, gateway: true}}, nil
		},
		Own: func(tenant, agent string) []tool.Tool {
			return []tool.Tool{stub{name: "memory_" + tenant + "_" + agent, access: tool.Own}}
		},
	}
	return f
}

func TestBuildMakesTheAgentFromItsDefinition(t *testing.T) {
	f := newFixture(t)
	a, d, err := f.b.Build(ctx, "t1", "payouts")
	if err != nil {
		t.Fatal(err)
	}
	want := registrytest.Definition("t1", "payouts")
	if a.Tenant != "t1" || a.ID != "payouts" || a.ModelID != want.Model || a.Price != want.Price ||
		a.MaxTokens != want.MaxTokens || a.Effort != want.Effort || a.System != want.Instructions || a.MaxCost != want.MaxCost || a.MaxTurns != want.MaxTurns {
		t.Fatalf("built %+v from %+v", a, want)
	}
	if d.History != want.History || d.Keep[session.Usage] != want.Keep[session.Usage] {
		t.Fatalf("returned definition %+v, want the stored one", d)
	}
	var names []string
	for _, tl := range a.Tools {
		names = append(names, tl.Spec().Name)
	}
	if got := strings.Join(names, ","); got != "stripe_refund,memory_t1_payouts" {
		t.Fatalf("tools %s, want the gateway's then its own", got)
	}
	if len(f.connects) != 1 || f.connects[0] != "https://gateway.test/mcp key-t1" {
		t.Fatalf("connected %v, want once to the gateway with t1's key", f.connects)
	}
}

func TestBuildFindsOnlyTheTenantsAgent(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.b.Build(ctx, "t2", "payouts"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("t2 built t1's agent: %v", err)
	}
	if f.secrets.calls != 0 || len(f.connects) != 0 {
		t.Fatal("a missing agent still looked up a key or connected")
	}
}

// A definition can't send its key to a gateway the server doesn't know.
func TestBuildRefusesAnUnknownGatewayBeforeTheKey(t *testing.T) {
	f := newFixture(t)
	d := registrytest.Definition("t1", "payouts")
	d.Gateway = "https://elsewhere.test/mcp"
	_ = f.agents.Put(ctx, d)
	if _, _, err := f.b.Build(ctx, "t1", "payouts"); !errors.Is(err, registry.ErrGateway) {
		t.Fatalf("got %v, want ErrGateway", err)
	}
	if f.secrets.calls != 0 || len(f.connects) != 0 {
		t.Fatal("the key was looked up, or sent, for a gateway that isn't allowed")
	}
}

// A store that returns another tenant's record doesn't get it built.
func TestBuildRefusesAnotherTenantsRecord(t *testing.T) {
	f := newFixture(t)
	f.b.Agents = wrong{f.agents}
	if _, _, err := f.b.Build(ctx, "t2", "payouts"); err == nil {
		t.Fatal("built t1's record for t2")
	}
	if f.secrets.calls != 0 {
		t.Fatal("looked up a key for another tenant's record")
	}
}

// wrong answers every tenant with t1's record.
type wrong struct{ *memory.Agents }

func (w wrong) Get(ctx context.Context, _, id string) (registry.Definition, error) {
	return w.Agents.Get(ctx, "t1", id)
}

func TestBuildRefusesWhatCantRun(t *testing.T) {
	t.Run("an invalid definition", func(t *testing.T) {
		f := newFixture(t)
		d := registrytest.Definition("t1", "payouts")
		d.MaxCost = 0
		_ = f.agents.Put(ctx, d)
		if _, _, err := f.b.Build(ctx, "t1", "payouts"); err == nil {
			t.Fatal("built an agent with no cost cap")
		}
		if f.secrets.calls != 0 {
			t.Fatal("looked up the key of an invalid definition")
		}
	})
	t.Run("a missing key", func(t *testing.T) {
		f := newFixture(t)
		delete(f.secrets.keys, [2]string{"t1", "agents/payouts"})
		if _, _, err := f.b.Build(ctx, "t1", "payouts"); err == nil || len(f.connects) != 0 {
			t.Fatalf("got %v and %d connects, want an error before connecting", err, len(f.connects))
		}
	})
	t.Run("a write tool outside the gateway", func(t *testing.T) {
		f := newFixture(t)
		f.b.Own = func(string, string) []tool.Tool { return []tool.Tool{stub{name: "send_email", access: tool.Write}} }
		if _, _, err := f.b.Build(ctx, "t1", "payouts"); !errors.Is(err, tool.ErrUngoverned) {
			t.Fatalf("got %v, want ErrUngoverned", err)
		}
	})
}

// BenchmarkBuild builds an agent per request as the server will: the
// definition from the store, the key, and the gateway's tools listed over MCP
// (initialize, then tools/list) against a gateway on this machine.
func BenchmarkBuild(b *testing.B) {
	srv := httptest.NewTLSServer(http.HandlerFunc(fakeGateway))
	defer srv.Close()
	agents := memory.NewAgents()
	d := registrytest.Definition("t1", "payouts")
	d.Gateway = srv.URL + "/mcp"
	if err := agents.Put(ctx, d); err != nil {
		b.Fatal(err)
	}
	builder := &registry.Builder{Agents: agents, Model: model0{}, Gateways: []string{d.Gateway},
		Secrets: &secrets{keys: map[[2]string]string{{"t1", d.KeyRef}: "key-t1"}},
		Connect: func(ctx context.Context, gateway, key string) ([]tool.Tool, error) {
			return kiff.New(gateway, key, srv.Client()).Tools(ctx)
		},
	}
	b.ReportAllocs()
	for b.Loop() {
		a, _, err := builder.Build(ctx, "t1", "payouts")
		if err != nil || len(a.Tools) != 20 {
			b.Fatalf("built %d tools: %v", len(a.Tools), err)
		}
	}
}

// fakeGateway answers initialize and lists 20 tools, as a Card's reach might.
func fakeGateway(w http.ResponseWriter, r *http.Request) {
	var msg struct {
		ID     *int64 `json:"id"`
		Method string `json:"method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if msg.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
	if msg.Method == "tools/list" {
		tools := make([]any, 20)
		for i := range tools {
			tools[i] = map[string]any{"name": "tool_" + string(rune('a'+i)), "description": "Does one thing for the company.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}}}
		}
		result = map[string]any{"tools": tools}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
}

// A workspace is opt-in per definition, and bound to the run being built
// (#86): an agent that doesn't ask for one never gets one, and one that does
// is never built without its run's.
func TestBuildGivesAWorkspaceOnlyWhenAskedAndOnlyForTheRun(t *testing.T) {
	f := newFixture(t)
	var gotTenant, gotRun string
	f.b.Workspace = func(tenant, run string) tool.Tool {
		gotTenant, gotRun = tenant, run
		return stub{name: "workspace", access: tool.Own}
	}
	has := func(a agent.Agent) bool {
		for _, tl := range a.Tools {
			if tl.Spec().Name == "workspace" {
				return true
			}
		}
		return false
	}

	a, _, err := f.b.BuildRun(ctx, "t1", "payouts", "run-1")
	if err != nil || has(a) || gotRun != "" {
		t.Fatalf("a definition without workspace: tool %v, built for %q, err %v", has(a), gotRun, err)
	}

	d := registrytest.Definition("t1", "payouts")
	d.Workspace = true
	if err := f.agents.Put(ctx, d); err != nil {
		t.Fatal(err)
	}
	a, _, err = f.b.BuildRun(ctx, "t1", "payouts", "run-1")
	if err != nil || !has(a) || gotTenant != "t1" || gotRun != "run-1" {
		t.Fatalf("with workspace: tool %v for %s/%s, err %v", has(a), gotTenant, gotRun, err)
	}
	if _, _, err := f.b.Build(ctx, "t1", "payouts"); !errors.Is(err, registry.ErrNoWorkspace) {
		t.Fatalf("built for no run: %v, want ErrNoWorkspace", err)
	}
	f.b.Workspace = nil
	if _, _, err := f.b.BuildRun(ctx, "t1", "payouts", "run-1"); !errors.Is(err, registry.ErrNoWorkspace) {
		t.Fatalf("a server with no workspaces: %v, want ErrNoWorkspace", err)
	}
}

// The show tool is the definition's to turn on; it is off unless set.
func TestShowIsOptIn(t *testing.T) {
	f := newFixture(t)
	has := func() bool {
		a, _, err := f.b.Build(ctx, "t1", "payouts")
		if err != nil {
			t.Fatal(err)
		}
		for _, tl := range a.Tools {
			if tl.Spec().Name == "show" {
				return true
			}
		}
		return false
	}
	if has() {
		t.Fatal("an agent got the show tool without asking for it")
	}
	d := registrytest.Definition("t1", "payouts")
	d.Show = true
	_ = f.agents.Put(ctx, d)
	if !has() {
		t.Fatal("an agent that asks for the show tool didn't get it")
	}
}

// A definition can't send its key to a planque the server doesn't allow;
// one it allows gets the planque tool, with the agent's own key.
func TestBuildGivesThePlanqueToolOnlyForAnAllowedPlanque(t *testing.T) {
	f := newFixture(t)
	var built []string
	f.b.Planques = []string{"https://planque.test"}
	f.b.Planque = func(base, tenant, key string) tool.Tool {
		built = append(built, base+" "+tenant+" "+key)
		return stub{name: "planque", access: tool.Own}
	}
	d := registrytest.Definition("t1", "payouts")
	d.Planque = "https://elsewhere.test"
	_ = f.agents.Put(ctx, d)
	if _, _, err := f.b.Build(ctx, "t1", "payouts"); !errors.Is(err, registry.ErrPlanque) {
		t.Fatalf("got %v, want ErrPlanque", err)
	}
	if f.secrets.calls != 0 || len(built) != 0 {
		t.Fatal("the key was looked up, or a planque tool built, for a planque that isn't allowed")
	}
	d.Planque = "https://planque.test"
	_ = f.agents.Put(ctx, d)
	a, _, err := f.b.Build(ctx, "t1", "payouts")
	if err != nil {
		t.Fatal(err)
	}
	if len(built) != 1 || !strings.HasPrefix(built[0], "https://planque.test t1 ") {
		t.Fatalf("planque tools built: %v", built)
	}
	found := false
	for _, tl := range a.Tools {
		found = found || tl.Spec().Name == "planque"
	}
	if !found {
		t.Fatal("the agent has no planque tool")
	}
}
