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

	"github.com/aws/aws-lambda-go/events"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/registry/registrytest"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/server/lambdaurl"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
)

// runner is a server.Runner on the memory stores, as the loop's will be: it
// stores the run and its session, and an Execute that records what it does
// and answers "done".
type runner struct {
	runs  *memory.Runs
	store *memory.Store

	mu        sync.Mutex
	starts    []server.Start
	execs     []string
	deadlines []time.Time   // each Execute's context deadline (zero: none)
	failing   error         // Execute fails with it, after the run started
	badCall   bool          // Execute first emits a tool call whose input isn't JSON
	hold      chan struct{} // when set, Execute waits on it after its first event
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
	} else if sess, _, err := rr.store.Read(ctx, s.Tenant, s.Session); err != nil || sess.Agent != s.Agent {
		return run.Run{}, session.ErrNotFound // the contract: another tenant's or agent's session isn't found
	}
	r := run.New(s.Tenant, s.Agent, s.Session, s.Trigger, s.Input, now)
	r.Source = s.Source
	return r, rr.runs.Create(ctx, r)
}

// Execute records the owner's message, says "do", "ne" as it goes, records
// its answer and completes. The client going away doesn't stop it: a run
// outlives its stream.
func (rr *runner) Execute(ctx context.Context, tenant, id string, emit func(agent.Event)) (run.Run, error) {
	rr.mu.Lock()
	rr.execs = append(rr.execs, id)
	dl, _ := ctx.Deadline()
	rr.deadlines = append(rr.deadlines, dl)
	hold := rr.hold
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
	record := func(seq int64, kind session.EventKind, text string) {
		ev := session.Event{Session: r.Session, Run: r.ID, Seq: seq, Time: now, Kind: kind, Body: json.RawMessage(fmt.Sprintf(`{"text":%q}`, text))}
		_ = rr.store.Append(ctx, tenant, r.Session, r.ID, []session.Event{ev})
	}
	_ = rr.store.Begin(ctx, tenant, r.Session, r.ID, now)
	record(1, session.OwnerMessage, r.Input)
	if hold != nil {
		<-hold
	}
	// A real run's next model or store call would fail on a cancelled
	// context: so does this one.
	if err := ctx.Err(); err != nil {
		_, _ = run.Change(context.WithoutCancel(ctx), rr.runs, r, now, func(n *run.Run) error { n.Status, n.Error = run.Error, err.Error(); return nil })
		return run.Run{}, err
	}
	if rr.badCall { // a turn cut off at its token limit leaves a call's input unfinished
		emit(agent.Event{Kind: agent.CallStart, CallID: "c1", Tool: "stripe_refund", Input: json.RawMessage(`{"charge":`)})
	}
	emit(agent.Event{Kind: agent.TextDelta, Text: "do"})
	emit(agent.Event{Kind: agent.TextDelta, Text: "ne"})
	if rr.failing != nil {
		_, _ = run.Change(ctx, rr.runs, r, now, func(n *run.Run) error { n.Status, n.Error = run.Error, "the model failed"; return nil })
		return run.Run{}, rr.failing
	}
	record(2, session.AgentMessage, "done")
	return run.Change(ctx, rr.runs, r, now, func(n *run.Run) error {
		n.Status, n.Stop, n.Output = run.Completed, "answered", "done"
		return nil
	})
}

type runFixture struct {
	*fixture
	server     *server.Server
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
	f.server = s
	base.srv.Close()
	base.srv = httptest.NewServer(s.Handler())
	t.Cleanup(base.srv.Close)
	return f
}

// noBackground serves the API with no background dispatcher.
func (f *runFixture) noBackground() {
	f.server.Background = nil
	f.restart()
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

// kinds splits a stream into its deltas' texts and its recorded events' ids.
func kinds(t *testing.T, events []sse) (deltas, ids []string) {
	t.Helper()
	for _, e := range events {
		switch e.name {
		case "delta":
			if e.id != "" {
				t.Fatalf("a delta has an id: %q", e.id)
			}
			deltas = append(deltas, fmt.Sprint(e.data["text"]))
		case "event":
			if e.id == "" || fmt.Sprint(e.data["seq"]) != e.id {
				t.Fatalf("a recorded event's id %q isn't its seq %v", e.id, e.data["seq"])
			}
			ids = append(ids, e.id)
		}
	}
	return deltas, ids
}

func TestStartStreamsTheRun(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	code, events := f.stream("t1", "POST", "/agents/payouts/runs", `{"input":"Pay the vendors"}`)
	if code != 200 || len(events) < 2 || events[0].name != "run" || events[len(events)-1].name != "end" {
		t.Fatalf("got %d %s", code, names(events))
	}
	if events[0].data["status"] != string(run.Pending) {
		t.Fatalf("first event %v", events[0].data)
	}
	deltas, ids := kinds(t, events)
	if strings.Join(deltas, ",") != "do,ne" || strings.Join(ids, ",") != "1,2" {
		t.Fatalf("deltas %v, recorded events %v; want do,ne and 1,2, each once", deltas, ids)
	}
	end := events[len(events)-1].data
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

// A client that loses the start stream resumes after the last id it got,
// and sees each recorded event once.
func TestStartThenResumeFromTheLastCursor(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.runner.hold = make(chan struct{})
	// Released once, here or when the test ends, so a failure can't leave
	// the run (and the server's shutdown) waiting.
	release := sync.OnceFunc(func() { close(f.runner.hold) })
	t.Cleanup(release)

	// Read the start stream until its first recorded event, then go away.
	sctx, cancel := context.WithCancel(ctx)
	req, _ := http.NewRequestWithContext(sctx, "POST", f.srv.URL+"/agents/payouts/runs", strings.NewReader(`{"input":"x"}`))
	req.Header.Set("X-Test-Tenant", "t1")
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var runID, cursor string
	sc := bufio.NewScanner(resp.Body)
	var name string
	timeout := time.AfterFunc(10*time.Second, cancel)
	for cursor == "" && sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: ") && name == "":
			cursor = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: ") && name == "run":
			var v map[string]any
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v)
			runID = fmt.Sprint(v["id"])
		case line == "":
			name = ""
		}
	}
	timeout.Stop()
	cancel()
	resp.Body.Close()
	if cursor != "1" || runID == "" {
		t.Fatalf("got cursor %q for run %q before going away", cursor, runID)
	}

	// The run goes on without the client, and ends.
	release()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, _ := f.runs.Get(ctx, "t1", runID)
		if got.Status == run.Completed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run is %s", got.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}

	code, events := f.stream("t1", "POST", "/agents/payouts/runs/"+runID+"/resume?after="+cursor, "")
	_, ids := kinds(t, events)
	if code != 200 || strings.Join(ids, ",") != "2" || events[len(events)-1].name != "end" {
		t.Fatalf("resumed after %s: %d %s, ids %v; want only 2, then end", cursor, code, names(events), ids)
	}
}

func TestStartInTheBackgroundWithNoDispatcher(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.noBackground()
	code, body := f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "x", "background": true})
	f.want(503, code, body, "a background start on a server with no dispatcher")
	if list, _ := f.runs.List(ctx, "t1", "payouts", 10); len(list) != 0 || len(f.runner.starts) != 0 {
		t.Fatalf("stored %d runs, started %d", len(list), len(f.runner.starts))
	}
}

// A session is one agent's thread: a run of another agent can't join it.
func TestStartRefusesAnotherAgentsSession(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.putAgent("t1", "refunds")
	f.session("t1", "refunds-thread", "refunds")
	code, body := f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "x", "session_id": "refunds-thread", "background": true})
	f.want(404, code, body, "a payouts run in the refunds thread")
	if len(f.runner.starts) != 0 {
		t.Fatal("the Runner was asked to start it")
	}
	// The same session for its own agent is fine.
	code, body = f.do("t1", "POST", "/agents/refunds/runs", map[string]any{"input": "x", "session_id": "refunds-thread", "background": true})
	f.want(202, code, body, "a refunds run in the refunds thread")
}

// The Runner's contract refuses another agent's session too, whoever calls it.
func TestRunnerContractRefusesAnotherAgentsSession(t *testing.T) {
	f := newRunFixture(t)
	f.session("t1", "refunds-thread", "refunds")
	_, err := f.runner.Start(ctx, server.Start{Tenant: "t1", Agent: "payouts", Session: "refunds-thread", Trigger: run.ByOwner, Input: "x"})
	if !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("got %v, want session.ErrNotFound", err)
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
	if code != 200 || len(events) == 0 || events[0].name != "run" {
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
	// A running run has a claim and a lease; neither is shown.
	running := f.stored(run.Running)
	code, body = f.do("t1", "GET", "/agents/payouts/runs/"+running.ID, nil)
	f.want(200, code, body, "get a running run")
	for _, k := range []string{"claim", "lease", "version"} {
		if _, ok := body[k]; ok {
			t.Errorf("a running run shows %q", k)
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

// keepFor is how long registrytest's agent keeps a session after its last
// event: its longest kept retention.
var keepFor = registrytest.Definition("t1", "x").Keep.SessionRetention()

// Every run the server ends itself expires by its agent's keep rules.
func TestRunsTheServerEndsExpire(t *testing.T) {
	if keepFor <= 0 {
		t.Fatal("the test agent keeps everything for good; this test needs a retention")
	}
	want := now.Add(keepFor)

	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	r := f.stored(run.Pending)
	code, body := f.do("t1", "POST", "/agents/payouts/runs/"+r.ID+"/cancel", nil)
	f.want(200, code, body, "cancel a pending run")
	if got, _ := f.runs.Get(ctx, "t1", r.ID); !got.Expires.Equal(want) {
		t.Fatalf("a cancelled pending run expires at %v, want %v", got.Expires, want)
	}

	f.bgFail = errors.New("throttled")
	code, body = f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "x", "background": true})
	f.want(500, code, body, "a background start that can't be dispatched")
	for _, got := range f.list("t1", "payouts") {
		if got.Status == run.Error && !got.Expires.Equal(want) {
			t.Fatalf("an undispatched run expires at %v, want %v", got.Expires, want)
		}
	}
}

func (f *runFixture) list(tenant, agent string) []run.Run {
	f.t.Helper()
	l, err := f.runs.List(ctx, tenant, agent, 100)
	if err != nil {
		f.t.Fatal(err)
	}
	return l
}

// racing is a run store where the run's claimant saves it just before each
// of the cancel's first `races` writes, as at a checkpoint.
type racing struct {
	*memory.Runs
	races int
}

func (r *racing) Put(ctx context.Context, prev, next run.Run) error {
	if r.races > 0 {
		r.races--
		cur, err := r.Runs.Get(ctx, prev.Tenant, prev.ID)
		if err != nil {
			return err
		}
		if _, err := run.Change(ctx, r.Runs, cur, now, func(n *run.Run) error { n.Turns++; return nil }); err != nil {
			return err
		}
	}
	return r.Runs.Put(ctx, prev, next)
}

func TestCancelReadsARunSavedMeanwhileAgain(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	r := f.stored(run.Running)
	race := &racing{Runs: f.runs, races: 1}
	f.server.Runs = race
	f.restart()
	code, body := f.do("t1", "POST", "/agents/payouts/runs/"+r.ID+"/cancel", nil)
	f.want(200, code, body, "a cancel that met a checkpoint")
	got, _ := f.runs.Get(ctx, "t1", r.ID)
	if !got.Cancel || got.Turns != 1 {
		t.Fatalf("got %+v; want Cancel set, and the claimant's save kept", got)
	}

	// A run that changes under every try is a conflict, in the end.
	r2 := f.stored(run.Running)
	race.races = cancelTries
	code, body = f.do("t1", "POST", "/agents/payouts/runs/"+r2.ID+"/cancel", nil)
	f.want(409, code, body, "a cancel that always meets a save")
}

// cancelTries mirrors the server's: how often a cancel reads a run again.
const cancelTries = 3

// restart serves the API again with the fixture's server as changed.
func (f *runFixture) restart() {
	f.srv.Close()
	f.srv = httptest.NewServer(f.server.Handler())
	f.t.Cleanup(f.srv.Close)
}

// A path the API has, asked with a method it doesn't take, is 405 with
// Allow; a path it doesn't have is 404. Both in JSON, like every error.
func TestWrongMethodAndUnknownPath(t *testing.T) {
	f := newRunFixture(t)
	for _, c := range []struct {
		method, path, allow string
		status              int
	}{
		{"PUT", "/health", "GET, HEAD", 405},
		{"DELETE", "/schedules/x/runs", "GET, HEAD", 405},
		{"PATCH", "/agents/payouts", "DELETE, GET, HEAD, PUT", 405},
		{"GET", "/nowhere", "", 404},
		{"GET", "/agents/payouts/runs/r1/nothing", "", 404},
	} {
		req, _ := http.NewRequest(c.method, f.srv.URL+c.path, nil)
		req.Header.Set("X-Test-Tenant", "t1")
		resp, err := f.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != c.status || err != nil || body["error"] == nil || resp.Header.Get("Allow") != c.allow {
			t.Errorf("%s %s: got %d Allow %q body %v (%v); want %d Allow %q and a JSON error",
				c.method, c.path, resp.StatusCode, resp.Header.Get("Allow"), body, err, c.status, c.allow)
		}
	}
}

// ctxSessions is the memory store reading as a networked store does: a
// cancelled context fails the read.
type ctxSessions struct{ *memory.Store }

func (c ctxSessions) State(ctx context.Context, tenant, sess, rn string) (session.RunState, error) {
	if err := ctx.Err(); err != nil {
		return session.RunState{}, err
	}
	return c.Store.State(ctx, tenant, sess, rn)
}

func (c ctxSessions) ReadRun(ctx context.Context, tenant, sess, rn string) (session.RunState, []session.Event, error) {
	if err := ctx.Err(); err != nil {
		return session.RunState{}, nil, err
	}
	return c.Store.ReadRun(ctx, tenant, sess, rn)
}

// A client that goes away mid-run leaves the run going and the log quiet:
// nothing is left to follow for it.
func TestADroppedClientIsNotFollowed(t *testing.T) {
	f := newRunFixture(t)
	f.server.Sessions = ctxSessions{f.store}
	f.restart()
	f.putAgent("t1", "payouts")
	f.runner.hold = make(chan struct{})
	release := sync.OnceFunc(func() { close(f.runner.hold) })
	t.Cleanup(release)

	sctx, cancel := context.WithCancel(ctx)
	req, _ := http.NewRequestWithContext(sctx, "POST", f.srv.URL+"/agents/payouts/runs", strings.NewReader(`{"input":"x"}`))
	req.Header.Set("X-Test-Tenant", "t1")
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = bufio.NewReader(resp.Body).ReadString('\n') // the stream has begun
	cancel()
	resp.Body.Close()

	time.Sleep(4 * followEvery) // the follower would have looked, and failed, a few times
	release()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if l := f.list("t1", "payouts"); len(l) == 1 && l[0].Status == run.Completed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run didn't complete without its client")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(2 * followEvery)
	if strings.Contains(f.logs.String(), "following a run") {
		t.Fatalf("the log filled after the client went away:\n%s", f.logs.String())
	}
}

// followEvery mirrors the server's: how often a stream looks for more of a run.
const followEvery = 500 * time.Millisecond

// The run keeps the invocation's deadline: the Runner's lease needs it.
func TestTheRunKeepsTheDeadline(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	dl := time.Now().Add(time.Minute).Truncate(time.Second)
	rctx, cancel := context.WithDeadline(ctx, dl)
	defer cancel()
	req := httptest.NewRequestWithContext(rctx, "POST", "/agents/payouts/runs", strings.NewReader(`{"input":"x"}`))
	req.Header.Set("X-Test-Tenant", "t1")
	w := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(w, req)
	if w.Code != 200 || len(f.runner.deadlines) != 1 || !f.runner.deadlines[0].Equal(dl) {
		t.Fatalf("got %d, deadlines %v; want the request's %v", w.Code, f.runner.deadlines, dl)
	}
}

// countingSessions counts how often the run is looked at.
type countingSessions struct {
	*memory.Store
	mu    sync.Mutex
	looks int
}

func (c *countingSessions) State(ctx context.Context, tenant, sess, rn string) (session.RunState, error) {
	c.mu.Lock()
	c.looks++
	c.mu.Unlock()
	return c.Store.State(ctx, tenant, sess, rn)
}

func (c *countingSessions) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.looks
}

// Through a Lambda function URL the request's context isn't cancelled when
// the client goes away: a failed write tells the stream instead, and the
// follower stops looking.
func TestAGoneClientStopsTheFollowerOnLambdaToo(t *testing.T) {
	f := newRunFixture(t)
	counting := &countingSessions{Store: f.store}
	f.server.Sessions = counting
	f.putAgent("t1", "payouts")
	f.runner.hold = make(chan struct{})
	release := sync.OnceFunc(func() { close(f.runner.hold) })
	t.Cleanup(release)

	e := events.LambdaFunctionURLRequest{RawPath: "/agents/payouts/runs", Body: `{"input":"x"}`,
		Headers:        map[string]string{"x-test-tenant": "t1"},
		RequestContext: events.LambdaFunctionURLRequestContext{HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "POST"}}}
	out, err := lambdaurl.Serve(ctx, f.server.Handler(), e, nil) // ctx is never cancelled
	if err != nil {
		t.Fatal(err)
	}
	_, _ = bufio.NewReader(out.Body).ReadString('\n') // the stream has begun
	_ = out.Close()                                   // the client goes away

	// While the run still waits, it records an event: the follower's write
	// of it fails, and from then on it must stop looking.
	runs := f.list("t1", "payouts")
	if len(runs) != 1 {
		t.Fatalf("%d runs", len(runs))
	}
	for wait := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) { // its first event is in
		if st, _ := f.store.State(ctx, "t1", runs[0].Session, runs[0].ID); st.Seq >= 1 {
			break
		}
		if time.Now().After(wait) {
			t.Fatal("the run never recorded its first event")
		}
	}
	ev := session.Event{Session: runs[0].Session, Run: runs[0].ID, Seq: 2, Time: now, Kind: session.AgentMessage, Body: json.RawMessage(`{"text":"x"}`)}
	if err := f.store.Append(ctx, "t1", runs[0].Session, runs[0].ID, []session.Event{ev}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * followEvery) // it looks, tries to send event 2, and fails
	before := counting.count()
	time.Sleep(4 * followEvery)
	if after := counting.count(); after != before {
		t.Fatalf("the follower looked %d more times after its client went away", after-before)
	}

	release() // the run carries on without its client
	deadline := time.Now().Add(10 * time.Second)
	for {
		if l := f.list("t1", "payouts"); len(l) == 1 && l[0].Status == run.Completed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run didn't complete without its client")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// An event that can't be encoded is sent as its fallback, and the stream
// carries on to its end.
func TestAnEventThatCantBeEncodedDoesntEndTheStream(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.runner.badCall = true
	code, events := f.stream("t1", "POST", "/agents/payouts/runs", `{"input":"x"}`)
	if code != 200 || len(events) == 0 || events[len(events)-1].name != "end" {
		t.Fatalf("got %d %s; want the stream to reach end", code, names(events))
	}
	var fallback bool
	var texts []string
	for _, e := range events {
		if e.name == "delta" && e.data["error"] != nil {
			fallback = true
		}
		if e.name == "delta" && e.data["text"] != nil {
			texts = append(texts, fmt.Sprint(e.data["text"]))
		}
	}
	if !fallback || strings.Join(texts, ",") != "do,ne" {
		t.Fatalf("events %s: fallback sent %v, texts after it %v; want the fallback, then do,ne", names(events), fallback, texts)
	}
}
