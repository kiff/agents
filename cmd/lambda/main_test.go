package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"

	"github.com/kiff/agents/registry/registrytest"
	"github.com/kiff/agents/schedule"
	"github.com/kiff/agents/schedule/scheduletest"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/server/lambdaurl"
	"github.com/kiff/agents/store/memory"
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

// newFunction wires the function as setup does, on memory stores.
func newFunction(t *testing.T) (*function, *scheduler, *memory.Agents) {
	t.Helper()
	agents, store := memory.NewAgents(), memory.New()
	sched := &scheduler{set: map[[2]string]schedule.Schedule{}}
	svc := &schedule.Service{Store: memory.NewSchedules(), Scheduler: sched,
		Live: func(ctx context.Context, tenant, agent string) (bool, error) {
			_, err := agents.Get(ctx, tenant, agent)
			return err == nil, nil
		},
		Start: func(context.Context, schedule.Firing) error { return errNoRuns },
	}
	auth, err := lambdaurl.Auth([]string{role})
	if err != nil {
		t.Fatal(err)
	}
	s := &server.Server{Auth: auth, Agents: agents, Sessions: store, Notes: store, Schedules: svc,
		Gateways: []string{"https://gateway.test/mcp"}}
	return &function{http: s.Handler(), schedules: svc, log: slog.New(slog.NewTextHandler(io.Discard, nil))}, sched, agents
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
	f, sched, agents := newFunction(t)
	_ = agents.Put(ctx, registrytest.Definition("t1", "payouts"))
	if err := f.schedules.Put(ctx, scheduletest.Schedule("t1", "morning")); err != nil {
		t.Fatal(err)
	}
	firing := func(id, due string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{"tenant": "t1", "schedule": id, "due": due})
		return b
	}
	// Due when the stored schedule fires: the run would start, and can't yet.
	if _, err := f.handle(ctx, firing("morning", "2026-10-19T07:00:00Z")); !errors.Is(err, errNoRuns) {
		t.Fatalf("got %v, want errNoRuns", err)
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
	for _, k := range []string{"AGENTS_TABLE", "AGENTS_CALLERS", "AGENTS_GATEWAYS", "SCHEDULE_GROUP", "SCHEDULE_TARGET", "SCHEDULE_ROLE"} {
		t.Setenv(k, "")
	}
	_, err := setup(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("set up with no configuration")
	}
	for _, k := range []string{"AGENTS_TABLE", "AGENTS_CALLERS", "SCHEDULE_GROUP", "SCHEDULE_TARGET", "SCHEDULE_ROLE"} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("the error doesn't name %s: %v", k, err)
		}
	}

	t.Setenv("AGENTS_TABLE", "agents")
	t.Setenv("SCHEDULE_GROUP", "agents")
	t.Setenv("SCHEDULE_TARGET", "the-function")
	t.Setenv("SCHEDULE_ROLE", "the-role")
	t.Setenv("AGENTS_CALLERS", "arn:aws:sts::000000000000:assumed-role/tafs-cloud") // no '/': would match tafs-cloud-evil
	if _, err := setup(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("set up with a caller role that could match others")
	}
	t.Setenv("AGENTS_CALLERS", role)
	t.Setenv("AWS_REGION", "eu-west-3")
	if _, err := setup(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("a complete configuration: %v", err)
	}
}
