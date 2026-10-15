package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
)

// runner is a server.Runner on the memory stores, as the loop's will be: it
// stores the run and its session, and an Execute that answers "done".
type runner struct {
	runs  *memory.Runs
	store *memory.Store

	mu      sync.Mutex
	starts  []server.Start
	execs   []string
	failing error // Execute fails with it, after the run started
}

func (rr *runner) Start(ctx context.Context, s server.Start) (run.Run, error) {
	rr.mu.Lock()
	rr.starts = append(rr.starts, s)
	rr.mu.Unlock()
	if s.Session == "" {
		s.Session = "s-" + run.NewID()
		if err := rr.store.Create(ctx, session.Session{ID: s.Session, Tenant: s.Tenant, Agent: s.Agent, Kind: session.KindChat, Started: now, Updated: now}); err != nil {
			return run.Run{}, err
		}
	} else if _, _, err := rr.store.Read(ctx, s.Tenant, s.Session); err != nil {
		return run.Run{}, err
	}
	r := run.New(s.Tenant, s.Agent, s.Session, s.Trigger, s.Input, now)
	r.Source = s.Source
	return r, rr.runs.Create(ctx, r)
}

func (rr *runner) Execute(ctx context.Context, tenant, id string, emit func(agent.Event)) (run.Run, error) {
	rr.mu.Lock()
	rr.execs = append(rr.execs, id)
	rr.mu.Unlock()
	if emit == nil {
		emit = func(agent.Event) {}
	}
	r, err := rr.runs.Get(ctx, tenant, id)
	if err != nil {
		return run.Run{}, err
	}
	if r, err = run.Change(ctx, rr.runs, r, now, func(n *run.Run) error { n.Status = run.Running; return nil }); err != nil {
		return r, err
	}
	emit(agent.Event{Kind: agent.TextDelta, Text: "do"})
	emit(agent.Event{Kind: agent.TextDelta, Text: "ne"})
	if rr.failing != nil {
		_, _ = run.Change(ctx, rr.runs, r, now, func(n *run.Run) error { n.Status, n.Error = run.Error, "the model failed"; return nil })
		return run.Run{}, rr.failing
	}
	return run.Change(ctx, rr.runs, r, now, func(n *run.Run) error {
		n.Status, n.Stop, n.Output = run.Completed, "answered", "done"
		return nil
	})
}

type runFixture struct {
	*fixture
	runs       *memory.Runs
	runner     *runner
	background []string
	bgFail     error
}

func newRunFixture(t *testing.T) *runFixture {
	t.Helper()
	f := &runFixture{runs: memory.NewRunsAt(func() time.Time { return now })}
	base := newFixture(t)
	f.fixture = base
	f.runner = &runner{runs: f.runs, store: base.store}
	s := &server.Server{
		Auth: func(r *http.Request) (server.Caller, error) {
			if t := r.Header.Get("X-Test-Tenant"); t != "" {
				return server.Caller{Tenant: t}, nil
			}
			return server.Caller{}, errors.New("no tenant header")
		},
		Agents: base.agents, Sessions: base.store, Notes: base.store, Schedules: base.schedSvc,
		Runs: f.runs, Runner: f.runner,
		Background: func(_ context.Context, tenant, id string) error {
			if f.bgFail != nil {
				return f.bgFail
			}
			f.background = append(f.background, tenant+"/"+id)
			return nil
		},
		Gateways: []string{"https://gateway.test/mcp"}, Now: func() time.Time { return now },
		Log: slog.New(slog.NewTextHandler(base.logs, nil)),
	}
	base.srv.Close()
	base.srv = httptest.NewServer(s.Handler())
	t.Cleanup(base.srv.Close)
	return f
}

// sse is one server-sent event.
type sse struct {
	name, id string
	data     map[string]any
}

// stream posts or gets path as tenant and reads the event stream to its end.
func (f *runFixture) stream(tenant, method, path, body string) (int, []sse) {
	f.t.Helper()
	// A stream that never ends fails here, not at the test binary's timeout.
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(sctx, method, f.srv.URL+path, strings.NewReader(body))
	req.Header.Set("X-Test-Tenant", tenant)
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		raw, _ := io.ReadAll(resp.Body)
		f.t.Logf("%s %s: %d %s", method, path, resp.StatusCode, raw)
		return resp.StatusCode, nil
	}
	var out []sse
	cur := sse{}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if sctx.Err() != nil {
			break
		}
		switch {
		case line == "":
			out = append(out, cur)
			cur = sse{}
		case strings.HasPrefix(line, "event: "):
			cur.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.data); err != nil {
				f.t.Fatalf("data %q: %v", line, err)
			}
		}
	}
	if sctx.Err() != nil {
		f.t.Fatalf("%s %s: the stream didn't end", method, path)
	}
	return resp.StatusCode, out
}

func names(events []sse) string {
	var n []string
	for _, e := range events {
		n = append(n, e.name)
	}
	return strings.Join(n, ",")
}

func TestStartStreamsTheRun(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	code, events := f.stream("t1", "POST", "/agents/payouts/runs", `{"input":"Pay the vendors"}`)
	if code != 200 || names(events) != "run,text,text,end" {
		t.Fatalf("got %d %s", code, names(events))
	}
	if events[0].data["status"] != string(run.Pending) || events[1].data["text"] != "do" {
		t.Fatalf("first events %v %v", events[0].data, events[1].data)
	}
	end := events[3].data
	if end["status"] != string(run.Completed) || end["output"] != "done" || end["id"] != events[0].data["id"] {
		t.Fatalf("end %v", end)
	}
	s := f.runner.starts[0]
	if s.Tenant != "t1" || s.Agent != "payouts" || s.Trigger != run.ByOwner || s.Input != "Pay the vendors" || s.Session != "" {
		t.Fatalf("started %+v", s)
	}
	if len(f.background) != 0 {
		t.Fatal("a streamed run was also sent to the background")
	}
}

func TestStartInTheBackground(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	code, body := f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "Pay the vendors", "background": true})
	f.want(202, code, body, "background start")
	if body["status"] != string(run.Pending) || len(f.background) != 1 || f.background[0] != "t1/"+body["id"].(string) {
		t.Fatalf("got %v, background %v", body, f.background)
	}
	if len(f.runner.execs) != 0 {
		t.Fatal("a background run was executed in the request")
	}
}

// A run nothing will claim is ended now, not left for the sweep.
func TestStartThatCantGoToTheBackground(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.bgFail = errors.New("throttled")
	code, body := f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "x", "background": true})
	f.want(500, code, body, "background start that fails")
	list, _ := f.runs.List(ctx, "t1", "payouts", 10)
	if len(list) != 1 || list[0].Status != run.Error {
		t.Fatalf("runs %+v, want one ERROR run", list)
	}
}

func TestStartRefuses(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.session("t2", "theirs", "payouts")
	for name, c := range map[string]struct {
		tenant, path, body string
		status             int
	}{
		"no input":                 {"t1", "/agents/payouts/runs", `{"input":""}`, 400},
		"an input too long":        {"t1", "/agents/payouts/runs", fmt.Sprintf(`{"input":%q}`, strings.Repeat("x", server.MaxInput+1)), 400},
		"an unknown field":         {"t1", "/agents/payouts/runs", `{"input":"x","tenant":"t2"}`, 400},
		"an agent it doesn't have": {"t1", "/agents/refunds/runs", `{"input":"x"}`, 404},
		"another tenant's agent":   {"t2", "/agents/payouts/runs", `{"input":"x"}`, 404},
		"another tenant's session": {"t1", "/agents/payouts/runs", `{"input":"x","session_id":"theirs"}`, 404},
		"no caller":                {"", "/agents/payouts/runs", `{"input":"x"}`, 401},
	} {
		code, body := f.do(c.tenant, "POST", c.path, c.body)
		if code != c.status {
			t.Errorf("%s: got %d %v, want %d", name, code, body, c.status)
		}
	}
	if list, _ := f.runs.List(ctx, "t1", "payouts", 10); len(list) != 0 || len(f.runner.execs) != 0 {
		t.Fatalf("a refused start stored %d runs, executed %d", len(list), len(f.runner.execs))
	}
}

// A run that fails ends its stream with the run as stored.
func TestStartThatFailsEndsWithTheStoredRun(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.runner.failing = errors.New("model down: secret detail")
	code, events := f.stream("t1", "POST", "/agents/payouts/runs", `{"input":"x"}`)
	if code != 200 || len(events) == 0 {
		t.Fatalf("got %d %s", code, names(events))
	}
	end := events[len(events)-1]
	if end.name != "end" || end.data["status"] != string(run.Error) || strings.Contains(fmt.Sprint(end.data), "secret detail") {
		t.Fatalf("end %s %v", end.name, end.data)
	}
	if !strings.Contains(f.logs.String(), "secret detail") {
		t.Fatal("the failure wasn't logged")
	}
}

// stored puts a run of t1's payouts in status, with a paused turn and a claim
// that the API must not show.
func (f *runFixture) stored(status run.Status) run.Run {
	f.t.Helper()
	f.session("t1", "s-"+string(status), "payouts")
	r := run.New("t1", "payouts", "s-"+string(status), run.ByOwner, "x", now)
	r.Status = status
	if status == run.Paused {
		r.Paused = &run.Turn{}
		r.Requirements = []run.Requirement{{CallID: "c1", Tool: "stripe_refund", Operation: "op1", Expires: now.Add(time.Hour)}}
	}
	if status == run.Running {
		r.Claim, r.Lease = "inv-1", now.Add(time.Hour)
	}
	if err := f.runs.Create(ctx, r); err != nil {
		f.t.Fatal(err)
	}
	return r
}

func TestGetRun(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	r := f.stored(run.Paused)
	code, body := f.do("t1", "GET", "/agents/payouts/runs/"+r.ID, nil)
	f.want(200, code, body, "get")
	if body["status"] != string(run.Paused) || len(body["requirements"].([]any)) != 1 {
		t.Fatalf("got %v", body)
	}
	for _, k := range []string{"paused", "claim", "lease", "version", "tenant"} {
		if _, ok := body[k]; ok {
			t.Errorf("shows %q", k)
		}
	}
	code, body = f.do("t2", "GET", "/agents/payouts/runs/"+r.ID, nil)
	f.want(404, code, body, "another tenant's run")
	code, body = f.do("t1", "GET", "/agents/refunds/runs/"+r.ID, nil)
	f.want(404, code, body, "the run under another agent")
}

func TestCancelRun(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	cases := []struct {
		status run.Status
		code   int
		after  run.Status
	}{
		{run.Pending, 200, run.Cancelled},
		{run.Running, 200, run.Running}, // stops at its next checkpoint
		{run.Paused, 409, run.Paused},   // its held calls must be withdrawn first (#15)
		{run.Completed, 409, run.Completed},
	}
	for _, c := range cases {
		r := f.stored(c.status)
		code, body := f.do("t2", "POST", "/agents/payouts/runs/"+r.ID+"/cancel", nil)
		f.want(404, code, body, "cancelling another tenant's run")
		code, body = f.do("t1", "POST", "/agents/payouts/runs/"+r.ID+"/cancel", nil)
		f.want(c.code, code, body, "cancel "+string(c.status))
		got, _ := f.runs.Get(ctx, "t1", r.ID)
		if got.Status != c.after {
			t.Errorf("%s: now %s, want %s", c.status, got.Status, c.after)
		}
		if c.status == run.Running && (!got.Cancel || body["cancel_requested"] != true) {
			t.Errorf("a running run wasn't asked to stop: %+v %v", got, body)
		}
	}
}

// record appends events 1..n to run r.
func (f *runFixture) record(r run.Run, from, to int64) {
	f.t.Helper()
	_ = f.store.Begin(ctx, "t1", r.Session, r.ID, now)
	var evs []session.Event
	for i := from; i <= to; i++ {
		evs = append(evs, session.Event{Session: r.Session, Run: r.ID, Seq: i, Time: now, Kind: session.AgentMessage,
			Body: json.RawMessage(fmt.Sprintf(`{"text":"e%d"}`, i))})
	}
	if err := f.store.Append(ctx, "t1", r.Session, r.ID, evs); err != nil {
		f.t.Fatal(err)
	}
}

func TestResumeReplaysThenFollows(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	r := f.stored(run.Running)
	f.record(r, 1, 3)
	go func() {
		// The run goes on while the client follows it.
		time.Sleep(700 * time.Millisecond)
		f.record(r, 4, 4)
		got, _ := f.runs.Get(ctx, "t1", r.ID)
		_, _ = run.Change(ctx, f.runs, got, now, func(n *run.Run) error { n.Status, n.Stop = run.Completed, "answered"; return nil })
	}()
	code, events := f.stream("t1", "POST", "/agents/payouts/runs/"+r.ID+"/resume?after=1", "")
	if code != 200 || names(events) != "event,event,event,end" {
		t.Fatalf("got %d %s", code, names(events))
	}
	var ids []string
	for _, e := range events[:3] {
		ids = append(ids, e.id)
	}
	if strings.Join(ids, ",") != "2,3,4" || events[3].data["status"] != string(run.Completed) {
		t.Fatalf("ids %v, end %v", ids, events[3].data)
	}

	code, body := f.do("t1", "POST", "/agents/payouts/runs/"+r.ID+"/resume?after=-1", nil)
	f.want(400, code, body, "a negative index")
	code, body = f.do("t2", "POST", "/agents/payouts/runs/"+r.ID+"/resume", nil)
	f.want(404, code, body, "resuming another tenant's run")
}

func TestResumeOfAStoppedRunEnds(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	r := f.stored(run.Paused)
	code, events := f.stream("t1", "POST", "/agents/payouts/runs/"+r.ID+"/resume", "")
	if code != 200 || names(events) != "end" || events[0].data["status"] != string(run.Paused) {
		t.Fatalf("got %d %s", code, names(events))
	}
}

func TestScheduleRuns(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	sc := scheduleBody("morning")
	code, body := f.do("t1", "POST", "/schedules", sc)
	f.want(201, code, body, "create schedule")
	for _, src := range []string{"morning", "morning", "evening"} {
		if _, err := f.runner.Start(ctx, server.Start{Tenant: "t1", Agent: "payouts", Trigger: run.BySchedule, Source: src, Input: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	code, body = f.do("t1", "GET", "/schedules/morning/runs", nil)
	f.want(200, code, body, "list")
	list := body["runs"].([]any)
	if len(list) != 2 || list[0].(map[string]any)["source"] != "morning" {
		t.Fatalf("listed %v", list)
	}
	code, body = f.do("t2", "GET", "/schedules/morning/runs", nil)
	f.want(404, code, body, "another tenant's schedule's runs")
}

func TestDeleteAgentDeletesItsRuns(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.putAgent("t1", "refunds")
	mine := f.stored(run.Completed)
	other := run.New("t1", "refunds", "s-other", run.ByOwner, "x", now)
	_ = f.runs.Create(ctx, other)
	code, body := f.do("t1", "DELETE", "/agents/payouts", nil)
	f.want(204, code, body, "delete")
	if _, err := f.runs.Get(ctx, "t1", mine.ID); !errors.Is(err, run.ErrNotFound) {
		t.Fatal("its run is still stored")
	}
	if _, err := f.runs.Get(ctx, "t1", other.ID); err != nil {
		t.Fatal("another agent's run was deleted")
	}
}

// Without a Runner, runs can't be started: the route isn't there.
func TestNoRunnerNoStart(t *testing.T) {
	f := newFixture(t) // a server with Runs and no Runner
	f.putAgent("t1", "payouts")
	code, body := f.do("t1", "POST", "/agents/payouts/runs", `{"input":"x"}`)
	f.want(404, code, body, "starting a run with no Runner")
}
