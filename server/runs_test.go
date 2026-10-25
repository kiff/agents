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
	"slices"
	"strconv"
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
	refusing  error         // Start refuses with it, as the runner refuses a run it won't start
	badCall   bool          // Execute first emits a tool call whose input isn't JSON
	hold      chan struct{} // when set, Execute waits on it after its first event
}

func (rr *runner) Start(ctx context.Context, s server.Start) (run.Run, error) {
	rr.mu.Lock()
	rr.starts = append(rr.starts, s)
	refusing := rr.refusing
	rr.mu.Unlock()
	if refusing != nil {
		return run.Run{}, refusing
	}
	opened := s.Session == "" // the contract: the run says whether it opened its session
	if opened {
		s.Session = "s-" + run.NewID()
		if err := rr.store.Create(ctx, session.Session{ID: s.Session, Tenant: s.Tenant, Agent: s.Agent, Kind: session.KindChat, Started: now, Updated: now, Owner: s.Requester.Key()}); err != nil {
			return run.Run{}, err
		}
	} else if sess, _, err := rr.store.Read(ctx, s.Tenant, s.Session); err != nil || sess.Agent != s.Agent || !sess.OwnedBy(s.Requester.Key()) {
		return run.Run{}, session.ErrNotFound // the contract: another tenant's, agent's or person's session isn't found
	}
	r := run.New(s.Tenant, s.Agent, s.Session, s.Trigger, s.Input, now)
	r.Source, r.AgentVersion, r.Draft, r.Requester, r.NewSession = s.Source, s.Version, s.Draft, s.Requester, opened
	if s.ID != "" { // the contract: a named run is stored once; again, it is the stored run with ErrExists
		r.ID = s.ID
		if got, err := rr.runs.Get(ctx, s.Tenant, s.ID); err == nil {
			return got, run.ErrExists
		}
	}
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
		{run.Paused, 409, run.Paused},   // nothing here withdraws its held calls (no Withdraw)
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

// The owner's message to a run that hasn't ended is kept on it, for its next
// checkpoint (#102): 202 with the run. An ended run, or one asked to stop,
// is a conflict, so the caller sends it as a new run; so is one full of
// messages. Another tenant's or agent's run isn't found.
func TestSteerRun(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.putAgent("t1", "other")
	cases := []struct {
		status run.Status
		code   int
	}{
		{run.Pending, 202},
		{run.Running, 202},
		{run.Paused, 202},
		{run.Completed, 409},
		{run.Cancelled, 409},
	}
	for _, c := range cases {
		r := f.stored(c.status)
		path := "/agents/payouts/runs/" + r.ID + "/messages"
		code, body := f.do("t2", "POST", path, map[string]any{"text": "only the €20 one"})
		f.want(404, code, body, "another tenant's run")
		code, body = f.do("t1", "POST", "/agents/other/runs/"+r.ID+"/messages", map[string]any{"text": "only the €20 one"})
		f.want(404, code, body, "another agent's run")
		code, body = f.do("t1", "POST", path, map[string]any{"text": "only the €20 one"})
		f.want(c.code, code, body, "steer "+string(c.status))
		got, _ := f.runs.Get(ctx, "t1", r.ID)
		if c.code == 202 {
			if got.Status != c.status || !slices.Equal(got.Untaken(), []string{"only the €20 one"}) {
				t.Errorf("%s: stored %+v", c.status, got)
			}
			steering, _ := body["steering"].([]any)
			if len(steering) != 1 {
				t.Errorf("%s: the run shown has steering %v", c.status, body["steering"])
			}
		} else if len(got.Steering) != 0 {
			t.Errorf("%s: an ended run took a message", c.status)
		}
	}

	r := f.stored(run.Running)
	path := "/agents/payouts/runs/" + r.ID + "/messages"
	for _, text := range []string{"", "   "} {
		code, body := f.do("t1", "POST", path, map[string]any{"text": text})
		f.want(400, code, body, "an empty message")
	}
	if _, err := run.Change(ctx, f.runs, r, now, func(n *run.Run) error { n.Cancel = true; return nil }); err != nil {
		t.Fatal(err)
	}
	code, body := f.do("t1", "POST", path, map[string]any{"text": "wait"})
	f.want(409, code, body, "a run asked to stop")

	full := f.stored(run.Running)
	for range run.MaxSteering {
		if _, err := run.Steer(ctx, f.runs, "t1", full.ID, "more", now); err != nil {
			t.Fatal(err)
		}
	}
	code, body = f.do("t1", "POST", "/agents/payouts/runs/"+full.ID+"/messages", map[string]any{"text": "one too many"})
	f.want(409, code, body, "a run full of messages")
}

// heldOn puts a run of t1's payouts PAUSED on one held call per exception.
func (f *runFixture) heldOn(exceptions ...string) run.Run {
	f.t.Helper()
	r := f.stored(run.Paused)
	r, err := run.Change(ctx, f.runs, r, now, func(n *run.Run) error {
		n.Requirements = nil
		for _, ex := range exceptions {
			n.Requirements = append(n.Requirements, run.Requirement{CallID: "c-" + ex, Tool: "stripe_refund",
				Operation: "op-" + ex, Exception: ex, Expires: now.Add(time.Hour)})
		}
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

// withdrawals is a fake KIFF withdraw: what it was asked, and its answers by
// exception ("" with fail set: an error).
type withdrawals struct {
	asked   []string
	answers map[string]string
	fail    error
}

func (w *withdrawals) withdraw(_ context.Context, r run.Run, exception string) (string, error) {
	w.asked = append(w.asked, r.Tenant+"/"+r.ID+"/"+exception)
	if st, ok := w.answers[exception]; ok {
		return st, nil
	}
	return "", w.fail
}

// Cancelling a paused run withdraws each call KIFF holds, records what
// became of it, asks the run to stop and hands it on: it is resolved, so
// it is continued, and ends CANCELLED there.
func TestCancelAPausedRunWithdrawsItsHeldCalls(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	w := &withdrawals{answers: map[string]string{"exc_1": "invalidated", "exc_2": "approved"}}
	f.server.Withdraw = w.withdraw
	f.restart()
	r := f.heldOn("exc_1", "exc_2")

	code, body := f.do("t1", "POST", "/agents/payouts/runs/"+r.ID+"/cancel", nil)
	f.want(200, code, body, "cancel a paused run")
	if body["status"] != string(run.Paused) || body["cancel_requested"] != true {
		t.Fatalf("answered %v", body)
	}
	if want := []string{"t1/" + r.ID + "/exc_1", "t1/" + r.ID + "/exc_2"}; !slices.Equal(w.asked, want) {
		t.Fatalf("withdrew %v, want %v", w.asked, want)
	}
	got, _ := f.runs.Get(ctx, "t1", r.ID)
	if !got.Cancel || got.Requirements[0].Answer != "invalidated" || got.Requirements[1].Answer != "approved" ||
		!got.Requirements[0].Answered.Equal(now) {
		t.Fatalf("stored %+v", got)
	}
	if !slices.Equal(f.background, []string{"t1/" + r.ID}) {
		t.Fatalf("handed on %v, want the run once", f.background)
	}

	// Asked again, KIFF is asked again (it answers the same), and the run
	// stays as it is but for the answers' repeat.
	code, body = f.do("t1", "POST", "/agents/payouts/runs/"+r.ID+"/cancel", nil)
	f.want(200, code, body, "cancel it again")
}

// A paused run is not cancelled, and nothing is recorded, when KIFF can't be
// asked: no Withdraw, or a held call with no exception ID. KIFF failing
// partway is 500 with the cancel recorded; repeating it withdraws the rest.
// Background failing doesn't fail the cancel: the sweep continues the run.
func TestCancelAPausedRunNeedsKIFF(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	r := f.heldOn("exc_1")
	code, body := f.do("t1", "POST", "/agents/payouts/runs/"+r.ID+"/cancel", nil)
	f.want(409, code, body, "no Withdraw")

	w := &withdrawals{answers: map[string]string{"exc_1": "invalidated"}, fail: errors.New("KIFF down: secret detail")}
	f.server.Withdraw = w.withdraw
	f.restart()
	noID := f.stored(run.Paused) // its requirement has no exception
	code, body = f.do("t1", "POST", "/agents/payouts/runs/"+noID.ID+"/cancel", nil)
	f.want(409, code, body, "a held call with no exception ID")
	if len(w.asked) != 0 {
		t.Fatalf("withdrew %v before knowing every call could be", w.asked)
	}

	two := f.heldOn("exc_1", "exc_2")
	code, body = f.do("t1", "POST", "/agents/payouts/runs/"+two.ID+"/cancel", nil)
	if code != 500 || strings.Contains(fmt.Sprint(body), "secret detail") {
		t.Fatalf("KIFF failing: %d %v", code, body)
	}
	for _, id := range []string{r.ID, noID.ID} {
		if got, _ := f.runs.Get(ctx, "t1", id); got.Cancel || !got.Requirements[0].Answered.IsZero() {
			t.Fatalf("a refused cancel changed the run: %+v", got)
		}
	}
	// KIFF failing partway: the cancel is recorded first, and what was
	// withdrawn is recorded, so the run can only end CANCELLED.
	if got, _ := f.runs.Get(ctx, "t1", two.ID); !got.Cancel || got.Requirements[0].Answer != "invalidated" || !got.Requirements[1].Answered.IsZero() {
		t.Fatalf("after a withdrawal failed partway: %+v", got)
	}

	w.fail = nil
	w.answers["exc_2"] = "refused"
	f.bgFail = errors.New("invoke failed")
	code, body = f.do("t1", "POST", "/agents/payouts/runs/"+two.ID+"/cancel", nil)
	f.want(200, code, body, "cancel once KIFF is back, with Background failing")
	if got, _ := f.runs.Get(ctx, "t1", two.ID); !got.Cancel {
		t.Fatalf("stored %+v", got)
	}
	if !strings.Contains(f.logs.String(), "invoke failed") {
		t.Fatal("the failed hand-on wasn't logged")
	}
}

// A withdrawal makes KIFF announce the hold invalidated, and the alert may
// record it and continue the run before the cancel returns: by then the run
// already carries Cancel, so it ends CANCELLED and takes no turn as if the
// owner had refused.
func TestACancelIsRecordedBeforeItsWithdrawals(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	r := f.heldOn("exc_1", "exc_2")
	alerted := 0
	f.server.Withdraw = func(ctx context.Context, rn run.Run, ex string) (string, error) {
		got, err := run.Answer(ctx, f.runs, rn.Tenant, ex, "invalidated", now) // KIFF's alert, landing first
		if err != nil {
			return "", err
		}
		if !got.Cancel {
			t.Errorf("KIFF's alert for %s found the run without Cancel: it would continue as a refusal", ex)
		}
		alerted++
		return "invalidated", nil
	}
	f.restart()
	code, body := f.do("t1", "POST", "/agents/payouts/runs/"+r.ID+"/cancel", nil)
	f.want(200, code, body, "cancel")
	if alerted != 2 || !slices.Equal(f.background, []string{"t1/" + r.ID}) {
		t.Fatalf("alerted %d, handed on %v", alerted, f.background)
	}
}

// A hold KIFF doesn't know for the agent's key (Withdraw's "", as for
// kiff.ErrNoHold) doesn't block the cancel or the agent's removal: there is
// nothing to take back, and its answer is KIFF's to send.
func TestAHoldKIFFDoesntKnowIsNothingToWithdraw(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	w := &withdrawals{answers: map[string]string{"exc_1": "invalidated", "exc_gone": ""}}
	f.server.Withdraw = w.withdraw
	f.restart()
	r := f.heldOn("exc_1", "exc_gone")
	code, body := f.do("t1", "POST", "/agents/payouts/runs/"+r.ID+"/cancel", nil)
	f.want(200, code, body, "cancel")
	got, _ := f.runs.Get(ctx, "t1", r.ID)
	if !got.Cancel || got.Requirements[0].Answer != "invalidated" || !got.Requirements[1].Answered.IsZero() || len(f.background) != 0 {
		t.Fatalf("stored %+v, handed on %v: want the unknown hold left to KIFF, and the run still waiting", got, f.background)
	}
	code, body = f.do("t1", "DELETE", "/agents/payouts", nil)
	f.want(204, code, body, "delete")
}

// answered records KIFF's answer on every requirement of r.
func (f *runFixture) answered(r run.Run) {
	f.t.Helper()
	r, _ = f.runs.Get(ctx, r.Tenant, r.ID)
	if _, err := run.Change(ctx, f.runs, r, now, func(n *run.Run) error {
		for i := range n.Requirements {
			n.Requirements[i].Answered, n.Requirements[i].Answer = now, "approved"
		}
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
}

// A paused run continues once KIFF has resolved it, streamed like a start,
// with only what it records from then on; before that, and once it went on,
// a continue is 409.
func TestContinueAPausedRun(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	r := f.stored(run.Paused)
	path := "/agents/payouts/runs/" + r.ID + "/continue"
	code, body := f.do("t1", "POST", path, nil)
	f.want(409, code, body, "a run still waiting for KIFF")
	code, body = f.do("t2", "POST", path, nil)
	f.want(404, code, body, "another tenant's run")
	f.record(r, 1, 3) // its turns before it paused
	f.answered(r)
	code, events := f.stream("t1", "POST", path, "")
	for _, e := range events {
		if e.name == "event" && (e.id == "1" || e.id == "2" || e.id == "3") {
			t.Fatalf("the continue replayed event %s, recorded before the pause: %s", e.id, names(events))
		}
	}
	if code != 200 || len(events) == 0 || events[0].name != "run" || events[0].data["status"] != string(run.Paused) {
		t.Fatalf("got %d %s", code, names(events))
	}
	if end := events[len(events)-1]; end.name != "end" || end.data["status"] != string(run.Completed) {
		t.Fatalf("ended %s %v", end.name, end.data)
	}
	if !slices.Contains(f.runner.execs, r.ID) {
		t.Fatalf("executed %v", f.runner.execs)
	}
	code, body = f.do("t1", "POST", path, nil)
	f.want(409, code, body, "a run that went on")
	code, body = f.do("t1", "POST", "/agents/payouts/runs/"+f.stored(run.Running).ID+"/continue", nil)
	f.want(409, code, body, "a running run")

	// Without a Runner, nothing here executes runs: the route isn't there.
	f.server.Runner = nil
	f.restart()
	other := f.heldResolved()
	code, body = f.do("t1", "POST", "/agents/payouts/runs/"+other.ID+"/continue", nil)
	f.want(404, code, body, "continuing with no Runner")
}

// heldResolved is a paused run of t1's payouts that KIFF has resolved.
func (f *runFixture) heldResolved() run.Run {
	f.t.Helper()
	r := f.stored(run.Paused)
	f.answered(r)
	return r
}

// {"background": true} hands the run on and answers 202; if the hand-off
// fails, the run stays paused for the sweep.
func TestContinueInTheBackground(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	r := f.stored(run.Paused)
	f.answered(r)
	path := "/agents/payouts/runs/" + r.ID + "/continue"
	code, body := f.do("t1", "POST", path, map[string]any{"background": true, "input": "x"})
	f.want(400, code, body, "a field continue doesn't take")
	f.bgFail = errors.New("invoke failed")
	code, body = f.do("t1", "POST", path, map[string]any{"background": true})
	f.want(500, code, body, "the hand-off failing")
	f.bgFail = nil
	code, body = f.do("t1", "POST", path, map[string]any{"background": true})
	f.want(202, code, body, "continue in the background")
	if !slices.Equal(f.background, []string{"t1/" + r.ID}) || len(f.runner.execs) != 0 {
		t.Fatalf("handed on %v, executed here %v", f.background, f.runner.execs)
	}
	if got, _ := f.runs.Get(ctx, "t1", r.ID); got.Status != run.Paused {
		t.Fatalf("now %s: the background invocation claims it, not the request", got.Status)
	}
	f.noBackground()
	code, body = f.do("t1", "POST", path, map[string]any{"background": true})
	f.want(503, code, body, "no background dispatcher")
}

// Work that came in starts one background run per event key: tagged as an
// event with its source, and started once however often it is delivered.
func TestAnEventStartsOneRun(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.putAgent("t1", "refunds")
	ev := map[string]any{"key": "evt_1", "source": "stripe", "input": "A dispute was opened on ch_1."}
	code, body := f.do("t1", "POST", "/agents/payouts/events", ev)
	f.want(202, code, body, "an event")
	id := server.EventRunID("t1", "payouts", "evt_1")
	if body["id"] != id || body["trigger"] != string(run.ByEvent) || body["source"] != "stripe" || body["status"] != string(run.Pending) {
		t.Fatalf("started %v", body)
	}
	if !slices.Equal(f.background, []string{"t1/" + id}) {
		t.Fatalf("handed on %v", f.background)
	}
	if got := f.runner.starts[len(f.runner.starts)-1]; got.Trigger != run.ByEvent || got.Session != "" || got.Input != ev["input"] {
		t.Fatalf("started with %+v: want an event run in a session of its own", got)
	}

	// Delivered again while it is still PENDING: handed on once more.
	code, body = f.do("t1", "POST", "/agents/payouts/events", ev)
	f.want(202, code, body, "the same event, still pending")
	if body["id"] != id || len(f.background) != 2 {
		t.Fatalf("got %v, handed on %v", body["id"], f.background)
	}
	// Once it went on: 200 with it, nothing started or handed on.
	if _, err := f.runner.Execute(ctx, "t1", id, nil); err != nil {
		t.Fatal(err)
	}
	code, body = f.do("t1", "POST", "/agents/payouts/events", ev)
	f.want(200, code, body, "the same event, its run gone on")
	if body["id"] != id || body["status"] != string(run.Completed) || len(f.background) != 2 {
		t.Fatalf("got %v, handed on %v", body, f.background)
	}

	ev["key"] = "evt_2"
	code, body = f.do("t1", "POST", "/agents/payouts/events", ev)
	f.want(202, code, body, "another event")
	if body["id"] == id {
		t.Fatal("another key started the same run")
	}
	code, body = f.do("t1", "POST", "/agents/refunds/events", ev)
	f.want(202, code, body, "the same key for another agent")
	if body["id"] == server.EventRunID("t1", "payouts", "evt_2") {
		t.Fatal("another agent's event is the same run")
	}
	code, body = f.do("t2", "POST", "/agents/payouts/events", ev)
	f.want(404, code, body, "another tenant's agent")
}

// An event the route can't take is 400, and nothing starts; with no
// background dispatcher it is 503; a failed hand-off is 500 and leaves the
// run PENDING for a redelivery.
func TestEventsThatDontStart(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	long := strings.Repeat("k", server.MaxEventKey+1)
	for name, ev := range map[string]map[string]any{
		"no key":            {"source": "stripe", "input": "x"},
		"a key too long":    {"key": long, "source": "stripe", "input": "x"},
		"no source":         {"key": "k", "input": "x"},
		"a source's case":   {"key": "k", "source": "Stripe", "input": "x"},
		"a source path":     {"key": "k", "source": "stripe/../x", "input": "x"},
		"no input":          {"key": "k", "source": "stripe"},
		"an input too long": {"key": "k", "source": "stripe", "input": strings.Repeat("x", server.MaxInput+1)},
		"another field":     {"key": "k", "source": "stripe", "input": "x", "session_id": "s1"},
	} {
		code, body := f.do("t1", "POST", "/agents/payouts/events", ev)
		f.want(400, code, body, name)
	}
	if len(f.runner.starts) != 0 {
		t.Fatalf("started %v", f.runner.starts)
	}

	ev := map[string]any{"key": "evt_1", "source": "stripe", "input": "x"}
	f.bgFail = errors.New("invoke failed")
	code, body := f.do("t1", "POST", "/agents/payouts/events", ev)
	f.want(500, code, body, "the hand-off failing")
	if got, _ := f.runs.Get(ctx, "t1", server.EventRunID("t1", "payouts", "evt_1")); got.Status != run.Pending {
		t.Fatalf("after a failed hand-off: %s, want PENDING for a redelivery", got.Status)
	}
	f.bgFail = nil
	code, body = f.do("t1", "POST", "/agents/payouts/events", ev)
	f.want(202, code, body, "redelivered")

	f.noBackground()
	code, body = f.do("t1", "POST", "/agents/payouts/events", ev)
	f.want(503, code, body, "no background dispatcher")
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

// Removing an agent withdraws the calls KIFF holds for its paused runs
// first, with the agent still there for its key; if KIFF can't be asked,
// nothing of the agent's is deleted.
func TestDeleteAgentWithdrawsItsHeldCalls(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	w := &withdrawals{answers: map[string]string{"exc_2": "invalidated"}, fail: errors.New("KIFF down")}
	f.server.Withdraw = func(ctx context.Context, r run.Run, ex string) (string, error) {
		if _, err := f.server.Agents.Get(ctx, r.Tenant, r.Agent); err != nil {
			t.Errorf("withdrawing after the agent was deleted: %v", err)
		}
		return w.withdraw(ctx, r, ex)
	}
	f.restart()
	r := f.heldOn("exc_1", "exc_2")
	f.stored(run.Completed)
	code, body := f.do("t1", "DELETE", "/agents/payouts", nil)
	f.want(500, code, body, "delete while KIFF is down")
	if _, err := f.runs.Get(ctx, "t1", r.ID); err != nil {
		t.Fatal("the paused run was deleted although its calls weren't withdrawn")
	}
	w.answers["exc_1"] = "invalidated"
	w.asked = nil
	code, body = f.do("t1", "DELETE", "/agents/payouts", nil)
	f.want(204, code, body, "delete")
	if want := []string{"t1/" + r.ID + "/exc_1", "t1/" + r.ID + "/exc_2"}; !slices.Equal(w.asked, want) {
		t.Fatalf("withdrew %v, want %v", w.asked, want)
	}
	if _, err := f.runs.Get(ctx, "t1", r.ID); !errors.Is(err, run.ErrNotFound) {
		t.Fatal("the paused run is still stored")
	}
}

// Without a Runner, runs can't be started: the routes aren't there.
func TestNoRunnerNoStart(t *testing.T) {
	f := newFixture(t) // a server with Runs and no Runner
	f.putAgent("t1", "payouts")
	// The path lists runs (GET), so starting one is a method it doesn't take.
	code, body := f.do("t1", "POST", "/agents/payouts/runs", `{"input":"x"}`)
	f.want(405, code, body, "starting a run with no Runner")
	code, body = f.do("t1", "POST", "/agents/payouts/events", map[string]any{"key": "k", "source": "stripe", "input": "x"})
	f.want(404, code, body, "an event with no Runner")
	code, body = f.do("t1", "POST", "/schedules", scheduleBody("morning"))
	f.want(201, code, body, "create schedule")
	code, body = f.do("t1", "POST", "/schedules/morning/trigger", nil)
	f.want(404, code, body, "triggering a schedule with no Runner")
}

// A schedule fired by hand starts an ordinary run of its agent with its input,
// tagged with the schedule, in the background.
func TestTriggerASchedule(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	sc := scheduleBody("morning")
	code, body := f.do("t1", "POST", "/schedules", sc)
	f.want(201, code, body, "create schedule")

	code, body = f.do("t1", "POST", "/schedules/morning/trigger", nil)
	f.want(202, code, body, "trigger")
	if body["status"] != string(run.Pending) || body["trigger"] != string(run.BySchedule) || body["source"] != "morning" ||
		body["agent"] != sc.Agent {
		t.Fatalf("got %v", body)
	}
	if len(f.background) != 1 || f.background[0] != "t1/"+body["id"].(string) || len(f.runner.execs) != 0 {
		t.Fatalf("background %v, executed in the request %d", f.background, len(f.runner.execs))
	}
	stored, err := f.runs.Get(ctx, "t1", body["id"].(string))
	if err != nil || stored.Input != sc.Input {
		t.Fatalf("stored %+v (%v), want the schedule's input", stored, err)
	}
	code, body = f.do("t1", "GET", "/schedules/morning/runs", nil)
	f.want(200, code, body, "its runs")
	if list := body["runs"].([]any); len(list) != 1 {
		t.Fatalf("listed %v", list)
	}

	code, body = f.do("t2", "POST", "/schedules/morning/trigger", nil)
	f.want(404, code, body, "another tenant's schedule")
	code, body = f.do("t1", "POST", "/schedules/evening/trigger", nil)
	f.want(404, code, body, "a schedule that doesn't exist")

	f.bgFail = errors.New("throttled")
	code, body = f.do("t1", "POST", "/schedules/morning/trigger", nil)
	f.want(500, code, body, "a trigger that can't go to the background")
	list, _ := f.runs.ListSource(ctx, "t1", run.BySchedule, "morning", 10)
	statuses := map[run.Status]int{}
	for _, rn := range list {
		statuses[rn.Status]++
	}
	if len(list) != 2 || statuses[run.Pending] != 1 || statuses[run.Error] != 1 {
		t.Fatalf("runs %+v, want the first PENDING and the second ERROR", list)
	}
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

// A tenant's active runs are capped: what would start one more is 429, with
// Retry-After, and starts nothing. Another tenant isn't held up; a run that
// ends frees its place; a delivery of an event already started still gets its
// run.
func TestATenantsActiveRunsAreCapped(t *testing.T) {
	f := newRunFixture(t)
	f.server.MaxActive = 2
	f.restart()
	f.putAgent("t1", "payouts")
	f.putAgent("t2", "payouts")
	code, body := f.do("t1", "POST", "/schedules", scheduleBody("morning"))
	f.want(201, code, body, "create schedule")
	paused := f.stored(run.Paused)
	f.answered(paused)

	ev := map[string]any{"key": "evt_1", "source": "stripe", "input": "A dispute was opened on ch_1."}
	code, body = f.do("t1", "POST", "/agents/payouts/events", ev)
	f.want(202, code, body, "an event, the first active run")
	first := body["id"].(string)
	start := map[string]any{"input": "Reconcile payouts.", "background": true}
	code, body = f.do("t1", "POST", "/agents/payouts/runs", start)
	f.want(202, code, body, "a run, the second")

	before, _ := f.runs.List(ctx, "t1", "payouts", 0)
	for _, c := range []struct {
		path string
		body any
	}{
		{"/agents/payouts/runs", start},
		{"/agents/payouts/runs", map[string]any{"input": "Reconcile payouts."}},
		{"/schedules/morning/trigger", nil},
		{"/agents/payouts/events", map[string]any{"key": "evt_2", "source": "stripe", "input": "Another dispute."}},
		{"/agents/payouts/runs/" + paused.ID + "/continue", map[string]any{"background": true}},
	} {
		code, body := f.do("t1", "POST", c.path, c.body)
		f.want(429, code, body, "at the cap: POST "+c.path)
		if msg, _ := body["error"].(string); !strings.Contains(msg, "2 runs going") {
			t.Errorf("POST %s: %q, want what to do", c.path, msg)
		}
	}
	if after, _ := f.runs.List(ctx, "t1", "payouts", 0); len(after) != len(before) {
		t.Fatalf("%d runs, then %d: a request at the cap started one", len(before), len(after))
	}
	if got, _ := f.runs.Get(ctx, "t1", paused.ID); got.Status != run.Paused {
		t.Fatalf("the paused run is %s: a continue at the cap went on", got.Status)
	}

	req, _ := http.NewRequest("POST", f.srv.URL+"/agents/payouts/runs", strings.NewReader(`{"input":"x","background":true}`))
	req.Header.Set("X-Test-Tenant", "t1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 429 || res.Header.Get("Retry-After") != strconv.Itoa(server.RetryAfter) {
		t.Fatalf("%d, Retry-After %q", res.StatusCode, res.Header.Get("Retry-After"))
	}

	code, body = f.do("t1", "POST", "/agents/payouts/events", ev)
	f.want(202, code, body, "the started event delivered again, at the cap")
	if body["id"] != first {
		t.Fatalf("got %v, want the event's own run", body["id"])
	}
	code, body = f.do("t2", "POST", "/agents/payouts/runs", start)
	f.want(202, code, body, "another tenant")

	if _, err := f.runner.Execute(ctx, "t1", first, nil); err != nil {
		t.Fatal(err)
	}
	code, body = f.do("t1", "POST", "/agents/payouts/runs/"+paused.ID+"/continue", map[string]any{"background": true})
	f.want(202, code, body, "a place freed")
}

// A refused run says so, with its category, where the API shows a run (#68).
func TestARefusedRunShowsWhy(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.session("t1", "s-refused", "payouts")
	r := run.New("t1", "payouts", "s-refused", run.ByOwner, "x", now)
	r.Status, r.Stop, r.Refusal = run.Completed, "refused", "cyber"
	if err := f.runs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	code, body := f.do("t1", "GET", "/agents/payouts/runs/"+r.ID, nil)
	f.want(200, code, body, "get")
	if body["stop"] != "refused" || body["refusal"] != "cyber" {
		t.Fatalf("got %v", body)
	}
}

// A continue asks KIFF for the answers no alert has brought yet (kiff/tafs#206):
// what KIFF answered is recorded as an alert records it, and the run goes on
// once every hold is answered. A hold KIFF still holds keeps it waiting (409),
// and KIFF not answering is 503, the run untouched.
func TestAContinueAsksKIFFForItsAnswers(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	answers := map[string]string{"exc_1": "approved"}
	var askErr error
	asked := 0
	f.server.Answers = func(_ context.Context, rn run.Run) (map[string]string, error) {
		asked++
		if rn.Tenant != "t1" || rn.Agent != "payouts" {
			t.Errorf("asked for %s/%s", rn.Tenant, rn.Agent)
		}
		return answers, askErr
	}
	f.restart()
	r := f.heldOn("exc_1", "exc_2")
	path := "/agents/payouts/runs/" + r.ID + "/continue"

	code, body := f.do("t1", "POST", path, map[string]any{"background": true})
	f.want(409, code, body, "one hold still waiting in KIFF")
	got, _ := f.runs.Get(ctx, "t1", r.ID)
	if got.Status != run.Paused || got.Requirements[0].Answer != "approved" || !got.Requirements[1].Answered.IsZero() || len(f.background) != 0 {
		t.Fatalf("stored %+v, handed on %v: want exc_1 recorded and the run still waiting", got.Requirements, f.background)
	}

	askErr = errors.New("gateway down")
	code, body = f.do("t1", "POST", path, map[string]any{"background": true})
	f.want(503, code, body, "KIFF not answering")
	if again, _ := f.runs.Get(ctx, "t1", r.ID); again.Status != run.Paused || !again.Requirements[1].Answered.IsZero() || len(f.background) != 0 {
		t.Fatalf("stored %+v, handed on %v: KIFF not answering must leave the run as it was", again.Requirements, f.background)
	}
	askErr = nil

	answers["exc_2"] = "refused"
	code, body = f.do("t1", "POST", path, map[string]any{"background": true})
	f.want(202, code, body, "every hold answered in KIFF")
	got, _ = f.runs.Get(ctx, "t1", r.ID)
	if got.Requirements[1].Answer != "refused" || !slices.Equal(f.background, []string{"t1/" + r.ID}) {
		t.Fatalf("stored %+v, handed on %v", got.Requirements, f.background)
	}

	// By the held call, as the app that answered it knows it: the same run.
	answers["exc_5"] = "approved"
	held := f.heldOn("exc_5")
	code, body = f.do("t1", "POST", "/agents/payouts/holds/exc_5/continue", map[string]any{"background": true})
	f.want(202, code, body, "continue by the held call")
	if body["id"] != held.ID {
		t.Fatalf("continued %v, want run %s", body["id"], held.ID)
	}
	code, body = f.do("t1", "POST", "/agents/refunds/holds/exc_5/continue", map[string]any{"background": true})
	f.want(404, code, body, "another agent's held call")
	code, body = f.do("t2", "POST", "/agents/payouts/holds/exc_5/continue", map[string]any{"background": true})
	f.want(404, code, body, "another tenant's held call")

	// A run the alerts already resolved doesn't ask.
	before := asked
	other := f.heldResolved()
	code, body = f.do("t1", "POST", "/agents/payouts/runs/"+other.ID+"/continue", map[string]any{"background": true})
	f.want(202, code, body, "a run already resolved")
	if asked != before {
		t.Fatal("asked KIFF for a run its alerts had resolved")
	}
}

// An agent's runs list newest first; active=true keeps the ones not ended:
// what it is doing and waiting on (kiff/tafs#206).
func TestListRunsAndTheActiveOnes(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.putAgent("t1", "other")
	mk := func(agent string, st run.Status) run.Run {
		r := run.New("t1", agent, "s1", run.ByOwner, "x", now)
		r.Status = st
		if err := f.runs.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	done, paused, running := mk("payouts", run.Completed), mk("payouts", run.Paused), mk("payouts", run.Running)
	mk("other", run.Running)
	ids := func(body map[string]any) map[string]bool {
		out := map[string]bool{}
		for _, r := range body["runs"].([]any) {
			out[r.(map[string]any)["id"].(string)] = true
		}
		return out
	}
	code, body := f.do("t1", "GET", "/agents/payouts/runs", nil)
	f.want(200, code, body, "list")
	if got := ids(body); len(got) != 3 || !got[done.ID] {
		t.Fatalf("listed %v", got)
	}
	code, body = f.do("t1", "GET", "/agents/payouts/runs?active=true", nil)
	f.want(200, code, body, "active")
	if got := ids(body); len(got) != 2 || !got[paused.ID] || !got[running.ID] {
		t.Fatalf("active %v", got)
	}
	code, body = f.do("t1", "GET", "/agents/nobody/runs", nil)
	f.want(404, code, body, "another agent")
	code, body = f.do("t2", "GET", "/agents/payouts/runs", nil)
	f.want(404, code, body, "another tenant")
	code, body = f.do("t1", "GET", "/agents/payouts/runs?limit=0", nil)
	f.want(400, code, body, "a bad limit")
}

// Each person's chat with a shared agent is theirs alone (#219): a live
// message goes on in its requester's own recent chat, never another's; a
// person can't name, list or read another's; a quiet chat isn't taken up
// again; and the run says who asked.
func TestEachRequestersChatIsTheirOwn(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	live := func(who string) map[string]any {
		return map[string]any{"input": "refund ch_1", "background": true, "chat": "live", "requester": map[string]any{"kind": "member", "id": who}}
	}
	code, body := f.do("t1", "POST", "/agents/payouts/runs", live("ana"))
	f.want(202, code, body, "ana's first message")
	ana, _ := body["session"].(string)
	if req, _ := body["requester"].(map[string]any); req["kind"] != "member" || req["id"] != "ana" {
		t.Fatalf("the run's requester = %v", body["requester"])
	}
	if body["new_session"] != true {
		t.Fatalf("ana's first message: new_session = %v", body["new_session"])
	}
	code, body = f.do("t1", "POST", "/agents/payouts/runs", live("ana"))
	f.want(202, code, body, "ana again")
	if body["session"] != ana {
		t.Fatalf("ana's next message went to %v, want her live chat %s", body["session"], ana)
	}
	if v, ok := body["new_session"]; !ok || v != false {
		t.Fatalf("ana's message in her live chat: new_session = %v (present %v)", v, ok)
	}
	code, body = f.do("t1", "POST", "/agents/payouts/runs", live("luis"))
	f.want(202, code, body, "luis")
	luis, _ := body["session"].(string)
	if luis == "" || luis == ana {
		t.Fatalf("luis's message went to %q: ana's chat is %s", luis, ana)
	}

	// Naming another person's chat, listing or reading it: not found.
	code, body = f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "x", "background": true, "session_id": ana, "requester": map[string]any{"kind": "member", "id": "luis"}})
	f.want(404, code, body, "luis naming ana's chat")
	code, body = f.do("t1", "GET", "/sessions?agent_id=payouts&owner=member:luis", nil)
	f.want(200, code, body, "luis's sessions")
	listed, _ := body["sessions"].([]any)
	if len(listed) != 1 || listed[0].(map[string]any)["id"] != luis {
		t.Fatalf("luis's sessions = %v", listed)
	}
	code, body = f.do("t1", "GET", "/sessions/"+ana+"?owner=member:luis", nil)
	f.want(404, code, body, "luis reading ana's chat")
	code, body = f.do("t1", "GET", "/sessions/"+ana+"?owner=member:ana", nil)
	f.want(200, code, body, "ana reading her chat")

	// A chat quiet for longer than the live window isn't taken up again.
	quiet := session.Session{ID: "s-quiet", Tenant: "t1", Agent: "payouts", Kind: session.KindChat, Started: now.Add(-2 * time.Hour), Updated: now.Add(-time.Hour), Owner: "member:zoe"}
	if err := f.store.Create(ctx, quiet); err != nil {
		t.Fatal(err)
	}
	code, body = f.do("t1", "POST", "/agents/payouts/runs", live("zoe"))
	f.want(202, code, body, "zoe after an hour")
	if body["session"] == "s-quiet" {
		t.Fatal("a chat quiet for an hour was taken up again")
	}

	for _, c := range []struct {
		name string
		body map[string]any
	}{
		{"live with no requester", map[string]any{"input": "x", "chat": "live"}},
		{"live with a session", map[string]any{"input": "x", "chat": "live", "session_id": ana, "requester": map[string]any{"kind": "member", "id": "ana"}}},
		{"an unknown chat", map[string]any{"input": "x", "chat": "newest"}},
		{"an unknown requester kind", map[string]any{"input": "x", "requester": map[string]any{"kind": "boss", "id": "ana"}}},
		{"a requester id with a colon", map[string]any{"input": "x", "requester": map[string]any{"kind": "member", "id": "a:b"}}},
	} {
		code, body := f.do("t1", "POST", "/agents/payouts/runs", c.body)
		f.want(400, code, body, c.name)
	}
	code, body = f.do("t1", "GET", "/sessions?agent_id=payouts&owner=boss:ana", nil)
	f.want(400, code, body, "an unknown owner kind")
}

// A chat its owner archives is put away (kiff/tafs#82): their next live
// message starts a new chat, and another person can't archive it.
func TestAnArchivedChatIsPutAway(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	live := func(who string) map[string]any {
		return map[string]any{"input": "hi", "background": true, "chat": "live", "requester": map[string]any{"kind": "member", "id": who}}
	}
	code, body := f.do("t1", "POST", "/agents/payouts/runs", live("ana"))
	f.want(202, code, body, "ana")
	ana, _ := body["session"].(string)
	code, body = f.do("t1", "POST", "/sessions/"+ana+"/archive?owner=member:luis", nil)
	f.want(404, code, body, "luis archiving ana's chat")
	code, body = f.do("t1", "POST", "/sessions/"+ana+"/archive?owner=member:ana", nil)
	f.want(200, code, body, "ana archiving her chat")
	if sess, _ := body["session"].(map[string]any); sess["archived"] != true {
		t.Fatalf("archived = %v", body["session"])
	}
	code, body = f.do("t1", "POST", "/agents/payouts/runs", live("ana"))
	f.want(202, code, body, "ana after archiving")
	if body["session"] == ana {
		t.Fatal("a live message went on in the chat she archived")
	}
	code, body = f.do("t1", "POST", "/sessions/"+ana+"/unarchive?owner=member:ana", nil)
	f.want(200, code, body, "ana putting it back")
	if sess, _ := body["session"].(map[string]any); sess["archived"] == true {
		t.Fatalf("still archived: %v", body["session"])
	}
}

// The flood guard counts each requester's own messages: one person at the
// limit is refused, and another still writes (#219).
func TestTheFloodGuardIsPerRequester(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.server.FloodMessages = 2
	msg := func(who string) map[string]any {
		return map[string]any{"input": "hi", "background": true, "requester": map[string]any{"kind": "member", "id": who}}
	}
	for i := range 2 {
		code, body := f.do("t1", "POST", "/agents/payouts/runs", msg("ana"))
		f.want(202, code, body, fmt.Sprintf("ana's message %d", i+1))
	}
	code, body := f.do("t1", "POST", "/agents/payouts/runs", msg("ana"))
	f.want(429, code, body, "ana past her guard")
	if m, _ := body["error"].(string); !strings.Contains(m, "2 messages in 10 minutes") {
		t.Fatalf("refusal = %q", m)
	}
	code, body = f.do("t1", "POST", "/agents/payouts/runs", msg("luis"))
	f.want(202, code, body, "luis, while ana is at her guard")
	// A door's sender has the door's own caps: none here.
	for i := range 3 {
		code, body = f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "hi", "background": true, "requester": map[string]any{"kind": "door", "id": "d1-sender"}})
		f.want(202, code, body, fmt.Sprintf("a door's sender, message %d", i+1))
	}
}

// A message steered into a run counts toward its writer's flood guard, and a
// run another person asked for isn't theirs to steer (#219).
func TestSteeringIsEachRequesters(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.server.FloodMessages = 3
	who := func(id string) map[string]any { return map[string]any{"kind": "member", "id": id} }
	code, body := f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "hi", "background": true, "requester": who("ana")})
	f.want(202, code, body, "ana's run")
	id, _ := body["id"].(string)
	steer := func(by map[string]any) (int, map[string]any) {
		b := map[string]any{"text": "only the €20 one"}
		if by != nil {
			b["requester"] = by
		}
		return f.do("t1", "POST", "/agents/payouts/runs/"+id+"/messages", b)
	}

	code, body = steer(who("luis"))
	f.want(404, code, body, "luis steering ana's run")
	code, body = steer(map[string]any{"kind": "member", "id": "a|b"})
	f.want(400, code, body, "a bad requester")
	code, body = steer(who("ana"))
	f.want(202, code, body, "ana steering")
	if st, _ := body["steering"].([]any); len(st) != 1 || st[0].(map[string]any)["requester"].(map[string]any)["id"] != "ana" {
		t.Fatalf("steering = %v", body["steering"])
	}
	// Her run and one steer are two; a third message is her last.
	code, body = steer(who("ana"))
	f.want(202, code, body, "ana's third message")
	code, body = steer(who("ana"))
	f.want(429, code, body, "ana steering past her guard")
	code, body = f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "hi", "background": true, "requester": who("ana")})
	f.want(429, code, body, "ana's new run past her guard")
	code, body = f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "hi", "background": true, "requester": who("luis")})
	f.want(202, code, body, "luis meanwhile")

	// A run from before the window still holds messages steered in it.
	old := run.New("t1", "payouts", "s-old", run.ByOwner, "hi", now.Add(-time.Hour))
	old.Requester = run.Requester{Kind: run.ByMember, ID: "zoe"}
	if err := f.runs.Create(ctx, old); err != nil {
		t.Fatal(err)
	}
	id = old.ID
	for i := range 3 {
		code, body = steer(who("zoe"))
		f.want(202, code, body, fmt.Sprintf("zoe's steer %d into an older run", i+1))
	}
	code, body = steer(who("zoe"))
	f.want(429, code, body, "zoe past her guard, by steers alone")

	// Without a requester, steering is as before: no guard, any run.
	code, body = steer(nil)
	f.want(202, code, body, "a steer naming no one")

	// Stopping: another person's run is not found; one's own stops.
	code, body = f.do("t1", "POST", "/agents/payouts/runs/"+id+"/cancel", map[string]any{"requester": who("ana")})
	f.want(404, code, body, "ana stopping zoe's run")
	code, body = f.do("t1", "POST", "/agents/payouts/runs/"+id+"/cancel", map[string]any{"requester": map[string]any{"kind": "member", "id": "a|b"}})
	f.want(400, code, body, "stopping as a bad requester")
	code, body = f.do("t1", "POST", "/agents/payouts/runs/"+id+"/cancel", map[string]any{"requester": who("zoe")})
	f.want(200, code, body, "zoe stopping her run")
}
