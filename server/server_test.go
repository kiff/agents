package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/registry/registrytest"
	"github.com/kiff/agents/schedule"
	"github.com/kiff/agents/schedule/scheduletest"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/skill"
	"github.com/kiff/agents/store/memory"
)

var ctx = context.Background()

// now is a Friday morning in Paris.
var now = time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)

// scheduler records what is set to fire.
type scheduler struct {
	set map[[2]string]schedule.Schedule
}

func (s *scheduler) Set(_ context.Context, sc schedule.Schedule) error {
	s.set[[2]string{sc.Tenant, sc.ID}] = sc
	return nil
}

func (s *scheduler) Remove(_ context.Context, tenant, id string) error {
	delete(s.set, [2]string{tenant, id})
	return nil
}

type fixture struct {
	t        *testing.T
	srv      *httptest.Server
	agents   *memory.Agents
	store    *memory.Store
	sched    *scheduler
	schedSvc *schedule.Service
	skills   *memory.Skills
	logs     *bytes.Buffer
	// erased are the tenants Erase was asked for.
	erased []string
}

// newFixture serves the API over memory stores. A request's tenant is its
// X-Test-Tenant header: the stand-in for a verified caller.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, agents: memory.NewAgents(), store: memory.NewAt(func() time.Time { return now }),
		sched: &scheduler{set: map[[2]string]schedule.Schedule{}}, skills: memory.NewSkills(), logs: &bytes.Buffer{}}
	f.schedSvc = &schedule.Service{Store: memory.NewSchedules(), Scheduler: f.sched,
		Live: func(ctx context.Context, tenant, agent string) (bool, error) {
			_, err := f.agents.Get(ctx, tenant, agent)
			if errors.Is(err, registry.ErrNotFound) {
				return false, nil
			}
			return err == nil, err
		},
		Start: func(context.Context, schedule.Firing) error { return nil },
	}
	s := &server.Server{
		Auth: func(r *http.Request) (server.Caller, error) {
			if t := r.Header.Get("X-Test-Tenant"); t != "" {
				return server.Caller{Tenant: t}, nil
			}
			return server.Caller{}, errors.New("no tenant header")
		},
		Agents: f.agents, Sessions: f.store, Notes: f.store, Schedules: f.schedSvc, Runs: memory.NewRuns(),
		Gateways: []string{"https://gateway.test/mcp"}, Skills: f.skills,
		Erase: func(_ context.Context, tenant string) error {
			f.erased = append(f.erased, tenant)
			return nil
		},
		Now: func() time.Time { return now },
		Log: slog.New(slog.NewTextHandler(f.logs, nil)),
	}
	f.srv = httptest.NewServer(s.Handler())
	t.Cleanup(f.srv.Close)
	return f
}

// do sends a request as tenant (none when empty) and returns the status and
// the decoded body.
func (f *fixture) do(tenant, method, path string, body any) (int, map[string]any) {
	f.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		j, err := json.Marshal(b)
		if err != nil {
			f.t.Fatal(err)
		}
		rd = bytes.NewReader(j)
	}
	req, err := http.NewRequest(method, f.srv.URL+path, rd)
	if err != nil {
		f.t.Fatal(err)
	}
	if tenant != "" {
		req.Header.Set("X-Test-Tenant", tenant)
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			f.t.Fatalf("%s %s: body %q is not a JSON object", method, path, raw)
		}
	}
	return resp.StatusCode, out
}

// want checks a status, and shows the body when it is wrong.
func (f *fixture) want(status int, got int, body map[string]any, what string) {
	f.t.Helper()
	if got != status {
		f.t.Fatalf("%s: got %d %v, want %d", what, got, body, status)
	}
}

// agentBody is a definition as a client sends it: no tenant, no id.
func agentBody(id string) registry.Definition {
	d := registrytest.Definition("", id)
	d.Tenant, d.ID = "", ""
	return d
}

func (f *fixture) putAgent(tenant, id string) {
	f.t.Helper()
	code, body := f.do(tenant, "PUT", "/agents/"+id, agentBody(id))
	f.want(200, code, body, "put agent "+id)
}

func TestHealthNeedsNoCaller(t *testing.T) {
	f := newFixture(t)
	code, body := f.do("", "GET", "/health", nil)
	f.want(200, code, body, "health")
}

func TestEveryRouteNeedsAVerifiedCaller(t *testing.T) {
	f := newFixture(t)
	f.putAgent("t1", "payouts")
	for _, r := range [][2]string{
		{"GET", "/agents"}, {"GET", "/agents/payouts"}, {"PUT", "/agents/payouts"}, {"DELETE", "/agents/payouts"},
		{"GET", "/agents/payouts/memories"}, {"DELETE", "/agents/payouts/memories/x"},
		{"GET", "/sessions?agent_id=payouts"}, {"GET", "/sessions/s1"}, {"DELETE", "/sessions/s1"},
		{"POST", "/schedules"}, {"GET", "/schedules"}, {"GET", "/schedules/m"}, {"PUT", "/schedules/m"},
		{"DELETE", "/schedules/m"}, {"POST", "/schedules/m/enable"}, {"POST", "/schedules/m/disable"},
	} {
		code, body := f.do("", r[0], r[1], nil)
		f.want(401, code, body, r[0]+" "+r[1]+" with no caller")
	}
	if _, err := f.agents.Get(ctx, "t1", "payouts"); err != nil {
		t.Fatal("an unverified DELETE removed the agent")
	}
}

func TestPutAgentTakesTheTenantFromTheCaller(t *testing.T) {
	f := newFixture(t)
	f.putAgent("t1", "payouts")
	d, err := f.agents.Get(ctx, "t1", "payouts")
	if err != nil {
		t.Fatal(err)
	}
	if d.Tenant != "t1" || d.ID != "payouts" || d.MaxCost != agentBody("x").MaxCost {
		t.Fatalf("stored %+v", d)
	}

	code, body := f.do("t1", "GET", "/agents/payouts", nil)
	f.want(200, code, body, "get")
	if body["tenant"] != "t1" || body["id"] != "payouts" {
		t.Fatalf("got %v", body)
	}
	code, body = f.do("t2", "GET", "/agents/payouts", nil)
	f.want(404, code, body, "another tenant's agent")
	code, body = f.do("t1", "GET", "/agents", nil)
	f.want(200, code, body, "list")
	if list := body["agents"].([]any); len(list) != 1 {
		t.Fatalf("listed %v", list)
	}
	code, body = f.do("t2", "GET", "/agents", nil)
	f.want(200, code, body, "another tenant's list")
	if list := body["agents"].([]any); len(list) != 0 {
		t.Fatalf("t2 lists %v", list)
	}
}

func TestPutAgentRefuses(t *testing.T) {
	cases := map[string]struct {
		body   any
		status int
	}{
		"another tenant in the body": {func() any { d := agentBody("p"); d.Tenant = "t2"; return d }(), 400},
		"another id in the body":     {func() any { d := agentBody("p"); d.ID = "other"; return d }(), 400},
		"no cost cap":                {func() any { d := agentBody("p"); d.MaxCost = 0; return d }(), 400},
		"a gateway not on the list":  {func() any { d := agentBody("p"); d.Gateway = "https://elsewhere.test/mcp"; return d }(), 400},
		// A valid definition with more: refused for the extra, not for the rest.
		"an unknown field": {withValid(`,"admin":true}`), 400},
		"not JSON":         {`{`, 400},
		"two values":       {withValid(`} {}`), 400},
		"a body too large": {`{"instructions":"` + strings.Repeat("x", 5<<20) + `"}`, 413},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			code, body := f.do("t1", "PUT", "/agents/p", c.body)
			f.want(c.status, code, body, name)
			if body["error"] == "" {
				t.Fatal("no error message")
			}
			for _, tenant := range []string{"t1", "t2"} {
				if _, err := f.agents.Get(ctx, tenant, "p"); !errors.Is(err, registry.ErrNotFound) {
					t.Fatalf("stored a refused agent for %s", tenant)
				}
			}
		})
	}
}

// withValid is a valid definition's JSON, its closing brace replaced by end.
func withValid(end string) string {
	b, err := json.Marshal(agentBody("p"))
	if err != nil {
		panic(err)
	}
	return string(b[:len(b)-1]) + end
}

// A caller verified with no tenant is no caller.
func TestACallerWithNoTenantIsRefused(t *testing.T) {
	f := newFixture(t)
	s := &server.Server{Auth: func(*http.Request) (server.Caller, error) { return server.Caller{}, nil }, Agents: f.agents}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/agents")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("got %d, want 401", resp.StatusCode)
	}
}

// Deleting an agent removes everything kept for it, and nothing else.
func TestDeleteAgentRemovesWhatIsKeptForIt(t *testing.T) {
	f := newFixture(t)
	for _, p := range [][2]string{{"t1", "payouts"}, {"t1", "refunds"}, {"t2", "payouts"}} {
		f.putAgent(p[0], p[1])
		sc := scheduletest.Schedule(p[0], "morning-"+p[1])
		sc.Agent = p[1]
		if err := f.schedSvc.Put(ctx, sc); err != nil {
			t.Fatal(err)
		}
		f.session(p[0], "s-"+p[1], p[1])
		if err := f.store.PutNote(ctx, session.Note{Tenant: p[0], Agent: p[1], Name: "n", Text: "x", Updated: now}); err != nil {
			t.Fatal(err)
		}
	}
	code, body := f.do("t2", "DELETE", "/agents/refunds", nil)
	f.want(404, code, body, "deleting an agent the tenant doesn't have")

	code, body = f.do("t1", "DELETE", "/agents/payouts", nil)
	f.want(204, code, body, "delete")
	if _, err := f.agents.Get(ctx, "t1", "payouts"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatal("the definition is still stored")
	}
	if _, err := f.schedSvc.Store.Get(ctx, "t1", "morning-payouts"); !errors.Is(err, schedule.ErrNotFound) {
		t.Fatal("its schedule is still stored")
	}
	if _, ok := f.sched.set[[2]string{"t1", "morning-payouts"}]; ok {
		t.Fatal("its schedule still fires")
	}
	if _, _, err := f.store.Read(ctx, "t1", "s-payouts"); !errors.Is(err, session.ErrNotFound) {
		t.Fatal("its session is still stored")
	}
	if notes, _ := f.store.ListNotes(ctx, "t1", "payouts"); len(notes) != 0 {
		t.Fatal("its notes are still stored")
	}
	// Another agent, and another tenant's agent of the same name, are untouched.
	for _, p := range [][2]string{{"t1", "refunds"}, {"t2", "payouts"}} {
		if _, err := f.agents.Get(ctx, p[0], p[1]); err != nil {
			t.Fatalf("%s/%s was removed", p[0], p[1])
		}
		if _, ok := f.sched.set[[2]string{p[0], "morning-" + p[1]}]; !ok {
			t.Fatalf("%s/%s's schedule was removed", p[0], p[1])
		}
		if _, _, err := f.store.Read(ctx, p[0], "s-"+p[1]); err != nil {
			t.Fatalf("%s/%s's session was removed", p[0], p[1])
		}
		if notes, _ := f.store.ListNotes(ctx, p[0], p[1]); len(notes) != 1 {
			t.Fatalf("%s/%s's notes were removed", p[0], p[1])
		}
	}
	code, body = f.do("t1", "DELETE", "/agents/payouts", nil)
	f.want(404, code, body, "deleting it again")
}

// session stores a session of agent with one event.
func (f *fixture) session(tenant, id, agent string) {
	f.t.Helper()
	s := session.Session{ID: id, Tenant: tenant, Agent: agent, Kind: session.KindChat, Started: now, Updated: now}
	if err := f.store.Create(ctx, s); err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.Begin(ctx, tenant, id, "r1", now); err != nil {
		f.t.Fatal(err)
	}
	ev := session.Event{Session: id, Run: "r1", Seq: 1, Time: now, Kind: session.OwnerMessage, Body: json.RawMessage(`{"text":"hello"}`)}
	if err := f.store.Append(ctx, tenant, id, "r1", []session.Event{ev}); err != nil {
		f.t.Fatal(err)
	}
}

func TestMemories(t *testing.T) {
	f := newFixture(t)
	f.putAgent("t1", "payouts")
	for _, name := range []string{"vendors", "rules"} {
		_ = f.store.PutNote(ctx, session.Note{Tenant: "t1", Agent: "payouts", Name: name, Text: "x", Updated: now})
	}
	code, body := f.do("t1", "GET", "/agents/payouts/memories", nil)
	f.want(200, code, body, "list")
	if list := body["memories"].([]any); len(list) != 2 {
		t.Fatalf("listed %v", list)
	}
	code, body = f.do("t2", "GET", "/agents/payouts/memories", nil)
	f.want(404, code, body, "another tenant's agent's memories")
	code, body = f.do("t2", "DELETE", "/agents/payouts/memories/rules", nil)
	f.want(404, code, body, "forgetting another tenant's note")
	code, body = f.do("t1", "DELETE", "/agents/payouts/memories/rules", nil)
	f.want(204, code, body, "forget")
	if notes, _ := f.store.ListNotes(ctx, "t1", "payouts"); len(notes) != 1 || notes[0].Name != "vendors" {
		t.Fatalf("left %v", notes)
	}
}

// #219: a person reads the shared notes and their own; a note of theirs is
// forgotten by its scope, and made shared on purpose.
func TestEachPersonsMemories(t *testing.T) {
	f := newFixture(t)
	f.putAgent("t1", "payouts")
	put := func(scope, name, text string) {
		_ = f.store.PutNote(ctx, session.Note{Tenant: "t1", Agent: "payouts", Scope: scope, Name: name, Text: text, Updated: now, Session: "s1"})
	}
	put(session.Shared, "vendors", "x")
	put("member:ana", "tone", "short")
	put("member:ana", "vendors", "y")
	put("member:luis", "tone", "long")
	names := func(body map[string]any) (out []string) {
		for _, n := range body["memories"].([]any) {
			m := n.(map[string]any)
			scope, _ := m["scope"].(string)
			out = append(out, scope+"/"+m["name"].(string))
		}
		return out
	}

	code, body := f.do("t1", "GET", "/agents/payouts/memories?owner=member:ana", nil)
	f.want(200, code, body, "ana's view")
	if got := names(body); !slices.Equal(got, []string{"/vendors", "member:ana/tone", "member:ana/vendors"}) {
		t.Fatalf("ana sees %v", got)
	}
	code, body = f.do("t1", "GET", "/agents/payouts/memories", nil)
	f.want(200, code, body, "every note")
	if got := names(body); len(got) != 4 {
		t.Fatalf("all: %v", got)
	}
	for _, q := range []string{"owner=ana", "owner=member:a|b", "owner=boss:ana"} {
		code, body = f.do("t1", "GET", "/agents/payouts/memories?"+q, nil)
		f.want(400, code, body, q)
	}

	code, body = f.do("t1", "POST", "/agents/payouts/memories/tone/share", nil)
	f.want(400, code, body, "sharing without a scope")
	code, body = f.do("t1", "POST", "/agents/payouts/memories/missing/share?scope=member:ana", nil)
	f.want(404, code, body, "sharing a missing note")
	code, body = f.do("t2", "POST", "/agents/payouts/memories/tone/share?scope=member:ana", nil)
	f.want(404, code, body, "sharing another tenant's note")
	code, body = f.do("t1", "POST", "/agents/payouts/memories/vendors/share?scope=member:ana", nil)
	f.want(409, code, body, "sharing over a shared note that says otherwise")
	code, body = f.do("t1", "POST", "/agents/payouts/memories/tone/share?scope=member:ana", nil)
	f.want(200, code, body, "share")
	if body["scope"] != nil || body["requester"] != "member:ana" || body["session"] != "s1" || body["text"] != "short" {
		t.Fatalf("shared %v", body)
	}
	// Luis now reads it too, beside his own of that name.
	code, body = f.do("t1", "GET", "/agents/payouts/memories?owner=member:luis", nil)
	f.want(200, code, body, "luis's view")
	if got := names(body); !slices.Equal(got, []string{"/tone", "/vendors", "member:luis/tone"}) {
		t.Fatalf("luis sees %v", got)
	}

	code, body = f.do("t1", "DELETE", "/agents/payouts/memories/tone?scope=member:luis", nil)
	f.want(204, code, body, "luis forgets his")
	code, body = f.do("t1", "DELETE", "/agents/payouts/memories/tone?scope=luis", nil)
	f.want(400, code, body, "a bad scope")
	notes, _ := f.store.ListNotes(ctx, "t1", "payouts")
	if len(notes) != 3 || notes[0].Name != "tone" || notes[0].Scope != session.Shared || notes[2].Scope != "member:ana" {
		t.Fatalf("left %+v", notes)
	}
}

func TestSessions(t *testing.T) {
	f := newFixture(t)
	f.session("t1", "s1", "payouts")
	f.session("t1", "s2", "refunds")
	// What a chat kept of a read is only the runner's: no view shows it.
	kept := session.Event{Session: "s1", Run: "r1", Seq: 2, Time: now, Kind: session.KeptRead, Body: json.RawMessage(`{"sealed":"AAAA"}`), Expires: now.Add(time.Hour)}
	if err := f.store.Append(ctx, "t1", "s1", "r1", []session.Event{kept}); err != nil {
		t.Fatal(err)
	}

	code, body := f.do("t1", "GET", "/sessions", nil)
	f.want(400, code, body, "list without agent_id")
	for _, l := range []string{"0", "101", "x"} {
		code, body = f.do("t1", "GET", "/sessions?agent_id=payouts&limit="+l, nil)
		f.want(400, code, body, "limit "+l)
	}
	code, body = f.do("t1", "GET", "/sessions?agent_id=payouts", nil)
	f.want(200, code, body, "list")
	if list := body["sessions"].([]any); len(list) != 1 || list[0].(map[string]any)["id"] != "s1" {
		t.Fatalf("listed %v", list)
	}
	code, body = f.do("t2", "GET", "/sessions?agent_id=payouts", nil)
	f.want(200, code, body, "another tenant's list")
	if list := body["sessions"].([]any); len(list) != 0 {
		t.Fatalf("t2 lists %v", list)
	}

	code, body = f.do("t1", "GET", "/sessions/s1", nil)
	f.want(200, code, body, "get")
	if events := body["events"].([]any); len(events) != 1 || body["session"].(map[string]any)["id"] != "s1" {
		t.Fatalf("got %v", body)
	}
	code, body = f.do("t2", "GET", "/sessions/s1", nil)
	f.want(404, code, body, "another tenant's session")
	code, body = f.do("t2", "DELETE", "/sessions/s1", nil)
	f.want(404, code, body, "deleting another tenant's session")
	code, body = f.do("t1", "DELETE", "/sessions/s1", nil)
	f.want(204, code, body, "delete")
	if _, _, err := f.store.Read(ctx, "t1", "s1"); !errors.Is(err, session.ErrNotFound) {
		t.Fatal("still stored")
	}
	if _, _, err := f.store.Read(ctx, "t1", "s2"); err != nil {
		t.Fatal("deleted another session")
	}
}

// scheduleBody is a schedule as a client sends it: no tenant.
func scheduleBody(id string) schedule.Schedule {
	s := scheduletest.Schedule("", id)
	s.Tenant = ""
	return s
}

func TestSchedules(t *testing.T) {
	f := newFixture(t)
	f.putAgent("t1", "payouts")

	code, body := f.do("t1", "POST", "/schedules", scheduleBody("morning"))
	f.want(201, code, body, "create")
	// 09:00 on weekdays in Paris; it is Friday 10:00 there, so Monday.
	if body["tenant"] != "t1" || body["next"] != "2026-10-19T07:00:00Z" {
		t.Fatalf("created %v", body)
	}
	if s, ok := f.sched.set[[2]string{"t1", "morning"}]; !ok || !s.Enabled {
		t.Fatal("not set to fire")
	}
	code, body = f.do("t1", "POST", "/schedules", scheduleBody("morning"))
	f.want(409, code, body, "creating it again")

	code, body = f.do("t1", "GET", "/schedules/morning", nil)
	f.want(200, code, body, "get")
	code, body = f.do("t2", "GET", "/schedules/morning", nil)
	f.want(404, code, body, "another tenant's schedule")
	code, body = f.do("t2", "POST", "/schedules/morning/disable", nil)
	f.want(404, code, body, "switching off another tenant's schedule")

	code, body = f.do("t1", "POST", "/schedules/morning/disable", nil)
	f.want(200, code, body, "disable")
	if body["enabled"] != false || body["next"] != nil || f.sched.set[[2]string{"t1", "morning"}].Enabled {
		t.Fatalf("disabled: %v, set %+v", body, f.sched.set[[2]string{"t1", "morning"}])
	}
	code, body = f.do("t1", "POST", "/schedules/morning/enable", nil)
	f.want(200, code, body, "enable")
	if !f.sched.set[[2]string{"t1", "morning"}].Enabled {
		t.Fatal("not enabled")
	}

	upd := scheduleBody("")
	upd.ID, upd.Cron = "", "30 7 * * *"
	code, body = f.do("t1", "PUT", "/schedules/morning", upd)
	f.want(200, code, body, "update")
	if f.sched.set[[2]string{"t1", "morning"}].Cron != "30 7 * * *" {
		t.Fatal("the update wasn't set")
	}
	code, body = f.do("t1", "PUT", "/schedules/evening", upd)
	f.want(404, code, body, "updating a schedule that doesn't exist")

	other := scheduleBody("evening")
	other.Agent = "refunds"
	f.putAgent("t1", "refunds")
	code, body = f.do("t1", "POST", "/schedules", other)
	f.want(201, code, body, "create another")
	code, body = f.do("t1", "GET", "/schedules?agent_id=refunds", nil)
	f.want(200, code, body, "list by agent")
	if list := body["schedules"].([]any); len(list) != 1 || list[0].(map[string]any)["id"] != "evening" {
		t.Fatalf("listed %v", list)
	}
	code, body = f.do("t2", "GET", "/schedules", nil)
	f.want(200, code, body, "another tenant's list")
	if list := body["schedules"].([]any); len(list) != 0 {
		t.Fatalf("t2 lists %v", list)
	}

	code, body = f.do("t2", "DELETE", "/schedules/morning", nil)
	f.want(404, code, body, "deleting another tenant's schedule")
	code, body = f.do("t1", "DELETE", "/schedules/morning", nil)
	f.want(204, code, body, "delete")
	if _, ok := f.sched.set[[2]string{"t1", "morning"}]; ok {
		t.Fatal("still fires")
	}
	code, body = f.do("t1", "GET", "/schedules/morning", nil)
	f.want(404, code, body, "after delete")
}

func TestCreateScheduleRefuses(t *testing.T) {
	cases := map[string]schedule.Schedule{
		"another tenant in the body": func() schedule.Schedule { s := scheduleBody("m"); s.Tenant = "t2"; return s }(),
		"an agent the tenant lacks":  func() schedule.Schedule { s := scheduleBody("m"); s.Agent = "chargebacks"; return s }(),
		"every five minutes":         func() schedule.Schedule { s := scheduleBody("m"); s.Cron = "*/5 * * * *"; return s }(),
		"an unknown zone":            func() schedule.Schedule { s := scheduleBody("m"); s.TimeZone = "Mars/Olympus"; return s }(),
	}
	for name, sc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.putAgent("t1", "payouts")
			code, body := f.do("t1", "POST", "/schedules", sc)
			f.want(400, code, body, name)
			if len(f.sched.set) != 0 {
				t.Fatal("set a refused schedule")
			}
			if list, _ := f.schedSvc.Store.List(ctx, "t1"); len(list) != 0 {
				t.Fatal("stored a refused schedule")
			}
		})
	}
}

// failing is an agent store whose every call fails.
type failing struct{ registry.Agents }

func (failing) Get(context.Context, string, string) (registry.Definition, error) {
	return registry.Definition{}, errors.New("dial tcp 10.0.0.7:443: secret detail")
}

// An error inside is logged; the caller reads only "internal error".
func TestInternalErrorsStayInside(t *testing.T) {
	f := newFixture(t)
	s := &server.Server{Auth: func(*http.Request) (server.Caller, error) { return server.Caller{Tenant: "t1"}, nil },
		Agents: failing{f.agents}, Log: slog.New(slog.NewTextHandler(f.logs, nil))}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/agents/payouts")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 500 || strings.Contains(string(raw), "secret detail") || !strings.Contains(string(raw), "internal error") {
		t.Fatalf("got %d %s", resp.StatusCode, raw)
	}
	if !strings.Contains(f.logs.String(), "secret detail") {
		t.Fatal("the error wasn't logged")
	}
}

// cutShort is a session store holding a session whose deletion was cut
// short: Read no longer finds it, and Delete finishes the job.
type cutShort struct {
	*memory.Store
	deleted bool
}

func (c *cutShort) Read(context.Context, string, string) (session.Session, []session.Event, error) {
	return session.Session{}, nil, session.ErrNotFound
}

func (c *cutShort) Delete(_ context.Context, tenant, id string) error {
	if tenant != "t1" || id != "s1" {
		return session.ErrNotFound
	}
	c.deleted = true
	return nil
}

// A delete that was cut short can be finished through the API.
func TestDeleteSessionFinishesACutShortDelete(t *testing.T) {
	store := &cutShort{Store: memory.New()}
	s := &server.Server{Auth: func(r *http.Request) (server.Caller, error) {
		return server.Caller{Tenant: r.Header.Get("X-Test-Tenant")}, nil
	}, Sessions: store}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	del := func(tenant string) int {
		req, _ := http.NewRequest("DELETE", srv.URL+"/sessions/s1", nil)
		req.Header.Set("X-Test-Tenant", tenant)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := del("t2"); code != 404 {
		t.Fatalf("another tenant got %d, want 404", code)
	}
	if code := del("t1"); code != 204 || !store.deleted {
		t.Fatalf("got %d (deleted %v), want 204 and the delete finished", code, store.deleted)
	}
}

// Deleting an account erases everything kept for it (kiff/tafs#185): every
// agent with its schedules, sessions and notes, the tenant's own halt, its
// skills, and what is kept outside the stores (Erase). Another tenant keeps
// all of its own, and asking again is done.
func TestEraseTenantDeletesEverythingKeptForIt(t *testing.T) {
	f := newFixture(t)
	for _, p := range [][2]string{{"t1", "payouts"}, {"t1", "refunds"}, {"t2", "payouts"}} {
		f.putAgent(p[0], p[1])
		sc := scheduletest.Schedule(p[0], "morning-"+p[1])
		sc.Agent = p[1]
		if err := f.schedSvc.Put(ctx, sc); err != nil {
			t.Fatal(err)
		}
		f.session(p[0], "s-"+p[0]+"-"+p[1], p[1])
		if err := f.store.PutNote(ctx, session.Note{Tenant: p[0], Agent: p[1], Name: "n", Text: "x", Updated: now}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tenant := range []string{"t1", "t2"} {
		if _, err := f.agents.Halt(ctx, tenant, registry.Tenant, now); err != nil {
			t.Fatal(err)
		}
		if err := f.skills.Put(ctx, skill.Package{Tenant: tenant, Name: "kiff", Version: "1"}); err != nil {
			t.Fatal(err)
		}
	}

	code, body := f.do("t1", "DELETE", "/tenant", nil)
	f.want(204, code, body, "erase")
	if defs, _ := f.agents.List(ctx, "t1"); len(defs) != 0 {
		t.Fatalf("agents left: %v", defs)
	}
	for _, a := range []string{"payouts", "refunds"} {
		if _, ok := f.sched.set[[2]string{"t1", "morning-" + a}]; ok {
			t.Fatalf("%s's schedule still fires", a)
		}
		if _, _, err := f.store.Read(ctx, "t1", "s-t1-"+a); !errors.Is(err, session.ErrNotFound) {
			t.Fatalf("%s's session is still stored", a)
		}
		if notes, _ := f.store.ListNotes(ctx, "t1", a); len(notes) != 0 {
			t.Fatalf("%s's notes are still stored", a)
		}
	}
	if h, _ := f.agents.Halted(ctx, "t1", registry.Tenant); h.On {
		t.Fatal("the tenant's halt is still stored")
	}
	if ps, _ := f.skills.List(ctx, "t1"); len(ps) != 0 {
		t.Fatalf("skills left: %v", ps)
	}
	if !slices.Equal(f.erased, []string{"t1"}) {
		t.Fatalf("Erase asked for %v, want t1", f.erased)
	}

	// The other tenant keeps everything.
	if _, err := f.agents.Get(ctx, "t2", "payouts"); err != nil {
		t.Fatal("t2's agent was removed")
	}
	if _, _, err := f.store.Read(ctx, "t2", "s-t2-payouts"); err != nil {
		t.Fatal("t2's session was removed")
	}
	if _, ok := f.sched.set[[2]string{"t2", "morning-payouts"}]; !ok {
		t.Fatal("t2's schedule was removed")
	}
	if h, _ := f.agents.Halted(ctx, "t2", registry.Tenant); !h.On {
		t.Fatal("t2's halt was removed")
	}
	if ps, _ := f.skills.List(ctx, "t2"); len(ps) != 1 {
		t.Fatal("t2's skills were removed")
	}

	code, body = f.do("t1", "DELETE", "/tenant", nil)
	f.want(204, code, body, "erasing again")
}
