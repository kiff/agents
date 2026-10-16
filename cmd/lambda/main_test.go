package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/registry/registrytest"
	"github.com/kiff/agents/registry/secretsmanager"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/runner"
	"github.com/kiff/agents/schedule"
	"github.com/kiff/agents/schedule/scheduletest"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/server/lambdaurl"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/kiff"
)

var ctx = context.Background()

// A made-up account: no real one is ever written here.
const role = "arn:aws:sts::000000000000:assumed-role/tafs-cloud/"

type scheduler struct {
	set     map[[2]string]schedule.Schedule
	removed []string
}

func (s *scheduler) Set(_ context.Context, sc schedule.Schedule) error {
	s.set[[2]string{sc.Tenant, sc.ID}] = sc
	return nil
}

func (s *scheduler) Remove(_ context.Context, tenant, id string) error {
	delete(s.set, [2]string{tenant, id})
	s.removed = append(s.removed, tenant+"/"+id)
	return nil
}

// newFunction wires the function as setup does, on memory stores, with a
// runner whose model says what m holds and whose background invocations go to
// inv.
func newFunction(t *testing.T) (*function, *scheduler, *memory.Agents) {
	f, sched, agents, _ := newFunctionWith(t, &replies{}, &invocations{})
	return f, sched, agents
}

func newFunctionWith(t *testing.T, m *replies, inv *invocations) (*function, *scheduler, *memory.Agents, *memory.Runs) {
	t.Helper()
	agents, store, runs := memory.NewAgents(), memory.New(), memory.NewRuns()
	sched := &scheduler{set: map[[2]string]schedule.Schedule{}}
	svc := &schedule.Service{Store: memory.NewSchedules(), Scheduler: sched,
		Live: func(ctx context.Context, tenant, agent string) (bool, error) {
			_, err := agents.Get(ctx, tenant, agent)
			return err == nil, nil
		},
	}
	auth, err := lambdaurl.Auth([]string{role})
	if err != nil {
		t.Fatal(err)
	}
	gateways := []string{"https://gateway.test/mcp"}
	rn := &runner.Runner{Runs: runs, Sessions: store, Builder: &registry.Builder{
		Agents: agents, Secrets: keys{}, Model: m, Gateways: gateways,
		Connect: func(context.Context, string, string) ([]tool.Tool, error) { return nil, nil },
	}}
	s := &server.Server{Auth: auth, Agents: agents, Sessions: store, Notes: store, Schedules: svc, Runs: runs,
		Runner: rn, Background: background(inv, "agents-fn"), Gateways: gateways}
	f := &function{http: s.Handler(), schedules: svc, runner: rn, sweeper: sweeping(runs, rn, background(inv, "agents-fn")),
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	svc.Start = f.fire
	return f, sched, agents, runs
}

func urlEvent(t *testing.T, method, path, signer, tenant string) json.RawMessage {
	t.Helper()
	e := events.LambdaFunctionURLRequest{Version: "2.0", RawPath: path, Headers: map[string]string{"kiff-tenant": tenant},
		RequestContext: events.LambdaFunctionURLRequestContext{HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: method}}}
	if signer != "" {
		e.RequestContext.Authorizer = &events.LambdaFunctionURLRequestContextAuthorizerDescription{
			IAM: &events.LambdaFunctionURLRequestContextAuthorizerIAMDescription{UserARN: signer}}
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestURLRequestsReachTheServer(t *testing.T) {
	f, _, agents := newFunction(t)
	_ = agents.Put(ctx, registrytest.Definition("t1", "payouts"))
	out, err := f.handle(ctx, urlEvent(t, "GET", "/agents/payouts", role+"fn", "t1"))
	if err != nil {
		t.Fatal(err)
	}
	resp := out.(*events.LambdaFunctionURLStreamingResponse)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"id":"payouts"`) {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	out, _ = f.handle(ctx, urlEvent(t, "GET", "/agents/payouts", "arn:aws:sts::000000000000:assumed-role/other/fn", "t1"))
	if resp := out.(*events.LambdaFunctionURLStreamingResponse); resp.StatusCode != 401 {
		t.Fatalf("another role got %d", resp.StatusCode)
	}
}

func TestFiringsReachTheSchedule(t *testing.T) {
	m := &replies{next: []model.Reply{answer("Payouts are reconciled.")}}
	f, sched, agents, runs := newFunctionWith(t, m, &invocations{})
	_ = agents.Put(ctx, registrytest.Definition("t1", "payouts"))
	if err := f.schedules.Put(ctx, scheduletest.Schedule("t1", "morning")); err != nil {
		t.Fatal(err)
	}
	firing := func(id, due string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{"tenant": "t1", "schedule": id, "due": due})
		return b
	}
	// Due when the stored schedule fires: its run starts, named by the
	// firing's key, and runs to its stop in this invocation.
	if _, err := f.handle(ctx, firing("morning", "2026-10-19T07:00:00Z")); err != nil {
		t.Fatal(err)
	}
	sc := scheduletest.Schedule("t1", "morning")
	key := schedule.Firing{Tenant: "t1", Schedule: "morning", Due: time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)}.Key()
	r, err := runs.Get(ctx, "t1", key)
	if err != nil || r.Status != run.Completed || r.Trigger != run.BySchedule || r.Source != "morning" ||
		r.Agent != sc.Agent || r.Input != sc.Input || r.Output != "Payouts are reconciled." {
		t.Fatalf("got %+v (%v)", r, err)
	}
	if list, _ := runs.ListSource(ctx, "t1", run.BySchedule, "morning", 10); len(list) != 1 {
		t.Fatalf("the schedule lists %d runs", len(list))
	}
	// Delivered again: no other run, and it isn't run again.
	if _, err := f.handle(ctx, firing("morning", "2026-10-19T07:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if list, _ := runs.ListSource(ctx, "t1", run.BySchedule, "morning", 10); len(list) != 1 || m.turns != 1 {
		t.Fatalf("a repeated firing: %d runs, %d model turns", len(list), m.turns)
	}
	// A schedule that is gone: removed from the Scheduler, and no error.
	if _, err := f.handle(ctx, firing("evening", "2026-10-19T07:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if strings.Join(sched.removed, ",") != "t1/evening" {
		t.Fatalf("removed %v", sched.removed)
	}
	if _, err := f.handle(ctx, firing("morning", "not a time")); err == nil {
		t.Fatal("accepted a firing with no due time")
	}
}

func TestOtherEventsAreRefused(t *testing.T) {
	f, _, _ := newFunction(t)
	for _, e := range []string{`{}`, `{"schedule":"x"}`, `{"requestContext":{}}`, `[]`, `"x"`} {
		if _, err := f.handle(ctx, json.RawMessage(e)); err == nil {
			t.Errorf("%s: accepted", e)
		}
	}
}

func TestSetupNeedsItsConfiguration(t *testing.T) {
	required := []string{"AGENTS_TABLE", "AGENTS_CALLERS", "SCHEDULE_GROUP", "SCHEDULE_TARGET", "SCHEDULE_ROLE",
		"AGENTS_SECRETS_PREFIX", "AGENTS_MODEL_KEY_SECRET", "AWS_LAMBDA_FUNCTION_NAME"}
	for _, k := range append(required, "AGENTS_GATEWAYS") {
		t.Setenv(k, "")
	}
	_, err := setup(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("set up with no configuration")
	}
	for _, k := range required {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("the error doesn't name %s: %v", k, err)
		}
	}

	t.Setenv("AGENTS_TABLE", "agents")
	t.Setenv("SCHEDULE_GROUP", "agents")
	t.Setenv("SCHEDULE_TARGET", "the-function")
	t.Setenv("SCHEDULE_ROLE", "the-role")
	t.Setenv("AGENTS_SECRETS_PREFIX", "agents/")
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "the-function")
	for _, bad := range []string{"agents/models/anthropic", "arn:aws:secretsmanager:eu-west-3:000000000000:secret:model-AbCdEf"} {
		t.Setenv("AGENTS_CALLERS", role)
		t.Setenv("AGENTS_MODEL_KEY_SECRET", bad)
		if _, err := setup(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
			t.Fatalf("set up with the model key at %s", bad)
		}
	}
	t.Setenv("AGENTS_MODEL_KEY_SECRET", "model/anthropic")
	t.Setenv("AGENTS_CALLERS", "arn:aws:sts::000000000000:assumed-role/tafs-cloud") // no '/': would match tafs-cloud-evil
	if _, err := setup(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("set up with a caller role that could match others")
	}
	t.Setenv("AGENTS_CALLERS", role)
	t.Setenv("AWS_REGION", "eu-west-3")
	f, err := setup(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("a complete configuration: %v", err)
	}
	// Runs are wired: the runner builds agents with keys from Secrets Manager
	// and a model keyed on first use, and the start route is served. A bad
	// body is refused before anything reaches AWS.
	rn, ok := f.runner.(*runner.Runner)
	if !ok || rn.Builder == nil {
		t.Fatalf("no runner: %T", f.runner)
	}
	if _, ok := rn.Builder.Secrets.(*secretsmanager.Secrets); !ok {
		t.Errorf("agents' keys from %T", rn.Builder.Secrets)
	}
	if _, ok := rn.Builder.Model.(*keyedModel); !ok {
		t.Errorf("the model is %T", rn.Builder.Model)
	}
	if rn.Collect == nil || reflect.ValueOf(rn.Collect).Pointer() != reflect.ValueOf(kiff.Collect).Pointer() {
		t.Error("a paused run must be continued by collecting its held calls from KIFF")
	}
	if f.sweeper == nil || f.cont == nil {
		t.Error("no sweep, or one that continues no paused run")
	}
	if f.api == nil || f.api.Withdraw == nil {
		t.Error("a paused run can't be cancelled: nothing withdraws its held calls from KIFF")
	}
	if f.api.MaxActive != defaultTenantRuns {
		t.Errorf("a tenant may have %d runs going, want %d by default", f.api.MaxActive, defaultTenantRuns)
	}
	for _, bad := range []string{"0", "-1", "ten", "2.5"} {
		t.Setenv("AGENTS_TENANT_RUNS", bad)
		if _, err := setup(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil || !strings.Contains(err.Error(), "AGENTS_TENANT_RUNS") {
			t.Errorf("set up with AGENTS_TENANT_RUNS %q: %v", bad, err)
		}
	}
	t.Setenv("AGENTS_TENANT_RUNS", "25")
	if f, err := setup(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil || f.api.MaxActive != 25 {
		t.Errorf("AGENTS_TENANT_RUNS 25: %v", err)
	}
	t.Setenv("AGENTS_TENANT_RUNS", "")
	if f.kiffAPI != kiff.APIURL {
		t.Errorf("held calls are withdrawn from %q, want KIFF's own %q", f.kiffAPI, kiff.APIURL)
	}
	// Gateways other than KIFF's: withdrawals need to know which KIFF.
	t.Setenv("AGENTS_GATEWAYS", "https://mcp.staging.example/mcp")
	if _, err := setup(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil || !strings.Contains(err.Error(), "AGENTS_KIFF_API") {
		t.Errorf("set up with other gateways and no AGENTS_KIFF_API: %v", err)
	}
	t.Setenv("AGENTS_KIFF_API", "https://api.staging.example/")
	if f, err := setup(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil || f.kiffAPI != "https://api.staging.example" {
		t.Errorf("withdrawing from %q (%v), want the configured KIFF", f.kiffAPI, err)
	}
	if f.schedules.Start == nil {
		t.Error("firings start nothing")
	}
	if status, body := startRun(t, f, "rae", `not json`); status != 400 {
		t.Errorf("start route: got %d %s", status, body)
	}
}

// A withdrawal sends the agent's KIFF key: only to an https host, with no
// path, query or credentials to bend where it goes.
func TestTheKIFFAPIWithdrawalsGoTo(t *testing.T) {
	for _, c := range []struct{ gateways, api, want string }{
		{"", "", kiff.APIURL},
		{"", "https://api.staging.example", "https://api.staging.example"},
		{"", " https://api.staging.example/ ", "https://api.staging.example"},
		{"https://mcp.staging.example/mcp", "https://api.staging.example", "https://api.staging.example"},
	} {
		if got, err := kiffAPIFor(c.gateways, c.api); err != nil || got != c.want {
			t.Errorf("%q, %q: %q (%v), want %q", c.gateways, c.api, got, err, c.want)
		}
	}
	for _, c := range []struct{ gateways, api string }{
		{"https://mcp.staging.example/mcp", ""},
		{"", "http://api.kiff.dev"},
		{"", "api.kiff.dev"},
		{"", "https://"},
		{"", "https://api.kiff.dev/v1"},
		{"", "https://api.kiff.dev?x=1"},
		{"", "https://api.kiff.dev#x"},
		{"", "https://user:pw@api.kiff.dev"},
		{"", "https://api.kiff.dev?"},
		{"", "https://api.kiff.dev#"},
		{"", "https://api.kiff.dev///"},
		{"", "https://api.kiff.dev//"},
		{"", "https://api.kiff.dev/%2F"},
		{"", "https:api.kiff.dev"},
	} {
		if got, err := kiffAPIFor(c.gateways, c.api); err == nil {
			t.Errorf("%q, %q: took %q", c.gateways, c.api, got)
		}
	}
}

// What kiffAPIFor takes, kiff.Withdraw reaches its route with: checked
// against a real server, since a 404 from a wrong route would read as no
// hold to withdraw.
func TestAWithdrawalReachesItsRoute(t *testing.T) {
	var paths []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hold":{"status":"invalidated"}}`))
	}))
	defer srv.Close()
	for _, api := range []string{srv.URL, srv.URL + "/", " " + srv.URL + " "} {
		paths = nil
		base, err := kiffAPIFor("", api)
		if err != nil {
			t.Fatalf("%q: %v", api, err)
		}
		if _, err := kiff.Withdraw(ctx, srv.Client(), base, "kiff-agent-key", "exc_123"); err != nil {
			t.Fatalf("%q: %v", api, err)
		}
		if len(paths) != 1 || paths[0] != "POST /v1/me/holds/exc_123/withdraw" {
			t.Fatalf("%q reached %v", api, paths)
		}
	}
}
