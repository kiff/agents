package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/registry/registrytest"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/runner"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
)

var ctx = context.Background()

// script is a fake model: it answers each turn with the next reply, and keeps
// the requests it was sent.
type script struct {
	mu      sync.Mutex
	replies []model.Reply
	reqs    []model.Request
}

func (s *script) Turn(ctx context.Context, req model.Request, onText func(string)) (model.Reply, error) {
	if err := ctx.Err(); err != nil {
		return model.Reply{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, req)
	if len(s.reqs) > len(s.replies) {
		return model.Reply{}, errors.New("script: no more replies")
	}
	r := s.replies[len(s.reqs)-1]
	for _, b := range r.Blocks {
		if b.Kind == model.Text && onText != nil {
			onText(b.Text)
		}
	}
	return r, nil
}

func (s *script) add(replies ...model.Reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replies = append(s.replies, replies...)
}

func (s *script) requests() []model.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs)
}

func say(text string) model.Reply {
	return model.Reply{Blocks: []model.Block{{Kind: model.Text, Text: text}}, Stop: model.EndTurn, Usage: model.Usage{InputTokens: 1000, OutputTokens: 100}}
}

func use(id, name, input string) model.Reply {
	return model.Reply{Blocks: []model.Block{{Kind: model.ToolUse, CallID: id, Tool: name, Input: json.RawMessage(input)}}, Stop: model.CallTools,
		Usage: model.Usage{InputTokens: 1000, OutputTokens: 100}}
}

// gatewayTool is a write tool through KIFF's gateway. Each call runs onCall,
// if set, and returns result.
type gatewayTool struct {
	name   string
	result tool.Result
	onCall func()
}

func (g *gatewayTool) Spec() model.ToolSpec {
	return model.ToolSpec{Name: g.name, InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (g *gatewayTool) Access() tool.Access { return tool.Write }
func (g *gatewayTool) Gateway() bool       { return true }
func (g *gatewayTool) Call(context.Context, json.RawMessage) (tool.Result, error) {
	if g.onCall != nil {
		g.onCall()
	}
	return g.result, nil
}

type keys struct{}

func (keys) Key(context.Context, string, string) (string, error) { return "key", nil }

// clock is a test clock that only moves when told to.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fixture is a runner on memory stores, with t1's agent "rae", a scripted
// model and one gateway tool, "stripe_refund".
type fixture struct {
	t      testing.TB
	clock  *clock
	runs   *memory.Runs
	store  *memory.Store
	agents *memory.Agents
	model  *script
	refund *gatewayTool
	r      *runner.Runner
	// connectErr, when set, is what connecting to the gateway fails with.
	connectErr error
}

func newFixture(t testing.TB, replies ...model.Reply) *fixture {
	t.Helper()
	c := &clock{t: time.Now().UTC().Truncate(time.Second)}
	f := &fixture{t: t, clock: c, runs: memory.NewRunsAt(c.now), store: memory.NewAt(c.now), agents: memory.NewAgents(),
		model: &script{replies: replies}, refund: &gatewayTool{name: "stripe_refund", result: tool.Result{Text: "refunded"}}}
	if err := f.agents.Put(ctx, registrytest.Definition("t1", "rae")); err != nil {
		t.Fatal(err)
	}
	f.r = f.newRunner()
	return f
}

// newRunner is a runner on the fixture's stores, as a fresh process would
// have one.
func (f *fixture) newRunner() *runner.Runner {
	return &runner.Runner{Runs: f.runs, Sessions: f.store, Now: f.clock.now,
		Builder: &registry.Builder{Agents: f.agents, Secrets: keys{}, Model: f.model, Gateways: []string{"https://gateway.test/mcp"},
			Connect: func(context.Context, string, string) ([]tool.Tool, error) {
				if f.connectErr != nil {
					return nil, f.connectErr
				}
				return []tool.Tool{f.refund}, nil
			}}}
}

func (f *fixture) start(s server.Start) run.Run {
	f.t.Helper()
	if s.Tenant == "" {
		s.Tenant, s.Agent, s.Trigger = "t1", "rae", run.ByOwner
	}
	f.clock.add(time.Second) // a session's runs are in the order they began
	r, err := f.r.Start(ctx, s)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *fixture) get(id string) run.Run {
	f.t.Helper()
	r, err := f.runs.Get(ctx, "t1", id)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *fixture) events(r run.Run) []session.EventKind {
	f.t.Helper()
	_, events, err := f.store.ReadRun(ctx, "t1", r.Session, r.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []session.EventKind
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

func (f *fixture) state(r run.Run) session.RunState {
	f.t.Helper()
	st, err := f.store.State(ctx, "t1", r.Session, r.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return st
}

func TestStartStoresAPendingRunInANewSession(t *testing.T) {
	f := newFixture(t)
	r := f.start(server.Start{Input: "refund order 42"})
	if r.Status != run.Pending || r.Input != "refund order 42" || r.Trigger != run.ByOwner || r.Session == "" {
		t.Fatalf("started %+v", r)
	}
	if got := f.get(r.ID); !run.Same(got, r) || got.Status != run.Pending {
		t.Fatalf("stored %+v", got)
	}
	s, _, err := f.store.Read(ctx, "t1", r.Session)
	if err != nil || s.Agent != "rae" || s.Kind != session.KindChat {
		t.Fatalf("the owner's run is in session %+v, %v; want a chat of rae's", s, err)
	}

	job := f.start(server.Start{Tenant: "t1", Agent: "rae", Trigger: run.BySchedule, Source: "sch-1", Input: "reconcile"})
	if job.Source != "sch-1" {
		t.Fatalf("a schedule's run has source %q", job.Source)
	}
	if s, _, _ := f.store.Read(ctx, "t1", job.Session); s.Kind != session.KindRun {
		t.Fatalf("a schedule's run is in a %s session, want a job's", s.Kind)
	}
}

func TestStartInTheConversationsSession(t *testing.T) {
	f := newFixture(t)
	first := f.start(server.Start{Input: "hello"})
	next := f.start(server.Start{Session: first.Session, Input: "and again"})
	if next.Session != first.Session || next.ID == first.ID {
		t.Fatalf("the second message is run %s in %s, want a new run in %s", next.ID, next.Session, first.Session)
	}
}

func TestStartRefuses(t *testing.T) {
	f := newFixture(t)
	if err := f.agents.Put(ctx, registrytest.Definition("t1", "other")); err != nil {
		t.Fatal(err)
	}
	if err := f.agents.Put(ctx, registrytest.Definition("t2", "rae")); err != nil {
		t.Fatal(err)
	}
	theirs := f.start(server.Start{Tenant: "t1", Agent: "other", Trigger: run.ByOwner, Input: "hi"})
	cases := map[string]struct {
		s    server.Start
		want error
	}{
		"another agent's session":  {server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Session: theirs.Session, Input: "x"}, session.ErrNotFound},
		"another tenant's session": {server.Start{Tenant: "t2", Agent: "rae", Trigger: run.ByOwner, Session: theirs.Session, Input: "x"}, session.ErrNotFound},
		"no such session":          {server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Session: "nope", Input: "x"}, session.ErrNotFound},
		"no such agent":            {server.Start{Tenant: "t1", Agent: "nobody", Trigger: run.ByOwner, Input: "x"}, registry.ErrNotFound},
		"no input":                 {server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner}, nil},
		"no trigger":               {server.Start{Tenant: "t1", Agent: "rae", Input: "x"}, nil},
	}
	before, _ := f.runs.List(ctx, "t1", "rae", 100)
	for name, c := range cases {
		_, err := f.r.Start(ctx, c.s)
		if err == nil || (c.want != nil && !errors.Is(err, c.want)) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
	if after, _ := f.runs.List(ctx, "t1", "rae", 100); len(after) != len(before) {
		t.Fatalf("refused starts stored %d runs", len(after)-len(before))
	}
}

func TestExecuteRunsToAnAnswer(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_refund", `{"order":42}`), say("Refunded order 42."))
	r := f.start(server.Start{Input: "refund order 42"})
	var text strings.Builder
	var kinds []agent.EventKind
	got, err := f.r.Execute(ctx, "t1", r.ID, func(e agent.Event) {
		kinds = append(kinds, e.Kind)
		text.WriteString(e.Text)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != run.Completed || got.Stop != string(agent.Answered) || got.Output != "Refunded order 42." ||
		got.Turns != 2 || got.Cost != 9000 || got.Claim != "" || !got.Lease.IsZero() {
		t.Fatalf("ended %+v", got)
	}
	// The definition keeps usage for 90 days, the longest of its rules.
	if want := f.clock.now().Add(90 * 24 * time.Hour); !got.Expires.Equal(want) {
		t.Fatalf("expires %v, want %v", got.Expires, want)
	}
	if stored := f.get(r.ID); stored.Version != got.Version || stored.Status != run.Completed {
		t.Fatalf("returned version %d, stored %+v", got.Version, stored)
	}
	if text.String() != "Refunded order 42." || !slices.Equal(kinds, []agent.EventKind{agent.CallStart, agent.CallResult, agent.TextDelta}) {
		t.Fatalf("streamed %v %q", kinds, text.String())
	}
	// The recording ended: tool calls, kept only until the end, are gone.
	want := []session.EventKind{session.OwnerMessage, session.Usage, session.Usage, session.AgentMessage}
	if got := f.events(r); !slices.Equal(got, want) {
		t.Fatalf("recorded %v, want %v", got, want)
	}
	if st := f.state(r); !st.Ended {
		t.Fatalf("recording %+v, want ended", st)
	}
}

// A job's last answer is its report.
func TestAJobReports(t *testing.T) {
	f := newFixture(t, say("All reconciled."))
	r := f.start(server.Start{Tenant: "t1", Agent: "rae", Trigger: run.BySchedule, Source: "sch-1", Input: "reconcile"})
	if _, err := f.r.Execute(ctx, "t1", r.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.events(r); !slices.Contains(got, session.Report) {
		t.Fatalf("recorded %v, want a report", got)
	}
}

func TestExecuteClaimsOnce(t *testing.T) {
	f := newFixture(t, say("done"))
	r := f.start(server.Start{Input: "go"})
	if _, err := f.r.Execute(ctx, "t1", r.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.newRunner().Execute(ctx, "t1", r.ID, nil); !errors.Is(err, run.ErrTransition) {
		t.Fatalf("executing a completed run again: %v", err)
	}
	if _, err := f.r.Execute(ctx, "t2", r.ID, nil); !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("another tenant executed the run: %v", err)
	}
	if n := len(f.model.requests()); n != 1 {
		t.Fatalf("the model was called %d times, want once", n)
	}
}

// A caller that goes away doesn't stop the run: its stream is picked back up
// with resume.
func TestExecuteOutlivesItsCaller(t *testing.T) {
	f := newFixture(t, say("done"))
	r := f.start(server.Start{Input: "go"})
	gone, cancel := context.WithCancel(ctx)
	cancel()
	got, err := f.r.Execute(gone, "t1", r.ID, nil)
	if err != nil || got.Status != run.Completed {
		t.Fatalf("a run whose caller left: %+v, %v", got, err)
	}
}

// The lease ends a minute after the caller's deadline.
func TestExecuteLeasesToTheDeadline(t *testing.T) {
	f := newFixture(t)
	deadline := f.clock.now().Add(3 * time.Minute)
	var lease time.Time
	f.model.add(use("c1", "stripe_refund", `{}`), say("done"))
	r := f.start(server.Start{Input: "go"})
	f.refund.onCall = func() { lease = f.get(r.ID).Lease }
	dctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if _, err := f.r.Execute(dctx, "t1", r.ID, nil); err != nil {
		t.Fatal(err)
	}
	if want := deadline.Add(run.LeaseGrace); !lease.Equal(want) {
		t.Fatalf("lease %v, want %v", lease, want)
	}
}

func TestHistoryIsTheSessionsFinishedRuns(t *testing.T) {
	f := newFixture(t, say("Hi."), say("Order 42 is paid."))
	first := f.start(server.Start{Input: "hello"})
	if _, err := f.r.Execute(ctx, "t1", first.ID, nil); err != nil {
		t.Fatal(err)
	}
	second := f.start(server.Start{Session: first.Session, Input: "is order 42 paid?"})
	if _, err := f.r.Execute(ctx, "t1", second.ID, nil); err != nil {
		t.Fatal(err)
	}
	// A run in another session, and one started but not run, aren't history.
	_ = f.start(server.Start{Input: "elsewhere"})
	_ = f.start(server.Start{Session: first.Session, Input: "not run yet"})

	f.model.add(say("Yes."))
	third := f.start(server.Start{Session: first.Session, Input: "sure?"})
	if _, err := f.r.Execute(ctx, "t1", third.ID, nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.model.requests()
	want := []string{"user: hello", "assistant: Hi.", "user: is order 42 paid?", "assistant: Order 42 is paid.", "user: sure?"}
	if got := texts(reqs[len(reqs)-1].Messages); !slices.Equal(got, want) {
		t.Fatalf("the third run saw %q, want %q", got, want)
	}
}

// A run still going in the session isn't history either.
func TestHistoryLeavesOutARunStillGoing(t *testing.T) {
	talk := use("c1", "stripe_refund", `{}`)
	talk.Blocks = append([]model.Block{{Kind: model.Text, Text: "Refunding it now."}}, talk.Blocks...)
	f := newFixture(t, talk)
	first := f.start(server.Start{Input: "refund order 42"})
	f.refund.onCall = func() {
		f.refund.onCall = nil
		// Recording is asynchronous: wait until what the run said is stored.
		for end := time.Now().Add(5 * time.Second); !slices.Contains(f.events(first), session.AgentMessage); time.Sleep(time.Millisecond) {
			if time.Now().After(end) {
				t.Error("the first run's message was never stored")
				return
			}
		}
		f.model.add(say("Hello."))
		next := f.start(server.Start{Session: first.Session, Input: "hi"})
		if _, err := f.r.Execute(ctx, "t1", next.ID, nil); err != nil {
			t.Error(err)
		}
	}
	f.model.add(say("Refunded."))
	if _, err := f.r.Execute(ctx, "t1", first.ID, nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.model.requests()
	if got := texts(reqs[1].Messages); !slices.Equal(got, []string{"user: hi"}) {
		t.Fatalf("saw %q, want no run still going in history", got)
	}
}

// Which runs came before a continued one doesn't depend on its events: the
// keep rules may have dropped them all while it was paused.
func TestHistoryWhenAPausedRunsEventsExpired(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_refund", `{}`), say("Waiting."))
	d := registrytest.Definition("t1", "rae")
	for k := range d.Keep {
		d.Keep[k] = session.Rule{Keep: session.Kept, Retention: time.Hour}
	}
	d.Keep[session.ToolCall], d.Keep[session.ToolResult] = session.Rule{Keep: session.Never}, session.Rule{Keep: session.Never}
	_ = f.agents.Put(ctx, d)
	r := f.start(server.Start{Input: "original request"})
	f.pause(r)
	f.clock.add(2 * time.Hour)
	if n := len(f.events(r)); n != 0 {
		t.Fatalf("the paused run still has %d events; the test needs them expired", n)
	}
	f.model.add(say("Later answer."))
	later := f.start(server.Start{Session: r.Session, Input: "later request"})
	if _, err := f.r.Execute(ctx, "t1", later.ID, nil); err != nil {
		t.Fatal(err)
	}
	f.answer(r)
	f.r.Collect = func(context.Context, agent.Agent, []run.Requirement) ([]agent.ToolResultBody, error) {
		return []agent.ToolResultBody{{CallID: "c1", Tool: "stripe_refund", Result: tool.Result{Text: "refunded"}}}, nil
	}
	f.model.add(say("Refunded."))
	if _, err := f.r.Execute(ctx, "t1", r.ID, nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.model.requests()
	if got := texts(reqs[len(reqs)-1].Messages); !slices.Equal(got, []string{"user: original request"}) {
		t.Fatalf("the continued run saw %q, want only its own input", got)
	}
}

func TestHistoryIsBoundedByTheDefinition(t *testing.T) {
	f := newFixture(t, say("one"), say("two"), say("three"))
	d := registrytest.Definition("t1", "rae")
	d.History = 1
	_ = f.agents.Put(ctx, d)
	r := f.start(server.Start{Input: "a"})
	for i, in := range []string{"a", "b", "c"} {
		if i > 0 {
			r = f.start(server.Start{Session: r.Session, Input: in})
		}
		if _, err := f.r.Execute(ctx, "t1", r.ID, nil); err != nil {
			t.Fatal(err)
		}
	}
	reqs := f.model.requests()
	if got, want := texts(reqs[2].Messages), []string{"user: b", "assistant: two", "user: c"}; !slices.Equal(got, want) {
		t.Fatalf("saw %q, want only the last finished run: %q", got, want)
	}
}

func texts(msgs []model.Message) []string {
	var out []string
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Kind == model.Text {
				out = append(out, string(m.Role)+": "+b.Text)
			}
		}
	}
	return out
}

// pause runs r into a hold on stripe_refund, and returns it as stored.
func (f *fixture) pause(r run.Run) run.Run {
	f.t.Helper()
	f.refund.result = tool.Result{Text: "KIFF holds this call for the owner", State: tool.StateHeld}
	got, err := f.r.Execute(ctx, "t1", r.ID, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	f.refund.result = tool.Result{Text: "refunded"}
	return got
}

// answer records KIFF's answer on every requirement, as its alert will.
func (f *fixture) answer(r run.Run) {
	f.t.Helper()
	r = f.get(r.ID)
	if _, err := run.Change(ctx, f.runs, r, f.clock.now(), func(n *run.Run) error {
		for i := range n.Requirements {
			n.Requirements[i].Answered = f.clock.now()
		}
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
}

func TestAHoldPausesTheRun(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_refund", `{"order":42}`), say("Waiting for the owner to approve the refund."))
	r := f.start(server.Start{Input: "refund order 42"})
	got := f.pause(r)
	if got.Status != run.Paused || got.Stop != string(agent.Paused) || got.Output != "Waiting for the owner to approve the refund." ||
		got.Claim != "" || got.Turns != 2 || !got.Expires.IsZero() {
		t.Fatalf("paused %+v", got)
	}
	if len(got.Requirements) != 1 || got.Requirements[0].CallID != "c1" || got.Requirements[0].Tool != "stripe_refund" ||
		string(got.Requirements[0].Input) != `{"order":42}` || !got.Requirements[0].Expires.IsZero() {
		t.Fatalf("requirements %+v", got.Requirements)
	}
	if got.Paused == nil || len(got.Paused.Reply.Blocks) != 1 {
		t.Fatalf("paused turn %+v", got.Paused)
	}
	s, _, _ := f.store.Read(ctx, "t1", r.Session)
	if st := f.state(r); !st.Held || !slices.Contains(s.Held, r.ID) {
		t.Fatalf("recording %+v in session %+v, want it held", st, s)
	}
	// Nothing collects held calls yet: it waits.
	if _, err := f.r.Execute(ctx, "t1", r.ID, nil); !errors.Is(err, runner.ErrNoCollect) {
		t.Fatalf("continuing with no Collect: %v", err)
	}
	if f.get(r.ID).Status != run.Paused {
		t.Fatal("a refused continue moved the run")
	}
}

// A paused run continues in a fresh process, from what the run stored alone.
func TestAPausedRunContinues(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_refund", `{"order":42}`), say("Waiting for approval."))
	r := f.start(server.Start{Input: "refund order 42"})
	paused := f.pause(r)

	fresh := f.newRunner()
	var asked []run.Requirement
	fresh.Collect = func(_ context.Context, a agent.Agent, held []run.Requirement) ([]agent.ToolResultBody, error) {
		asked = held
		return []agent.ToolResultBody{{CallID: "c1", Tool: "stripe_refund", Result: tool.Result{Text: "refunded, approved by the owner"}}}, nil
	}
	if _, err := fresh.Execute(ctx, "t1", r.ID, nil); !errors.Is(err, run.ErrNotReady) {
		t.Fatalf("continuing before KIFF answered: %v", err)
	}
	if asked != nil {
		t.Fatal("KIFF was asked before the hold was resolved")
	}
	f.answer(r)
	f.model.add(say("Refunded order 42."))
	got, err := fresh.Execute(ctx, "t1", r.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0].CallID != "c1" {
		t.Fatalf("collected %+v", asked)
	}
	if got.Status != run.Completed || got.Output != "Refunded order 42." || got.Turns != paused.Turns+1 ||
		got.Requirements != nil || got.Paused != nil || got.ID != r.ID {
		t.Fatalf("continued to %+v", got)
	}
	reqs := f.model.requests()
	last := reqs[len(reqs)-1].Messages
	if len(last) != 3 || last[2].Blocks[0].Result != "refunded, approved by the owner" {
		t.Fatalf("the model continued from %+v", last)
	}
	s, _, _ := f.store.Read(ctx, "t1", r.Session)
	if st := f.state(r); !st.Ended || len(s.Held) != 0 {
		t.Fatalf("recording %+v, session %+v: want ended and no longer held", st, s)
	}
}

// While a run is paused, a new message in its session starts a new run, and
// its history leaves the paused run out.
func TestANewMessageWhilePaused(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_refund", `{}`), say("Waiting."), say("Hello."))
	r := f.start(server.Start{Input: "refund order 42"})
	f.pause(r)
	next := f.start(server.Start{Session: r.Session, Input: "hi"})
	if _, err := f.r.Execute(ctx, "t1", next.ID, nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.model.requests()
	if got := texts(reqs[len(reqs)-1].Messages); !slices.Equal(got, []string{"user: hi"}) {
		t.Fatalf("saw %q, want no paused run in history", got)
	}

	// Continued, the paused run sees what came before it, not what came after.
	f.answer(r)
	f.r.Collect = func(context.Context, agent.Agent, []run.Requirement) ([]agent.ToolResultBody, error) {
		return []agent.ToolResultBody{{CallID: "c1", Tool: "stripe_refund", Result: tool.Result{Text: "refunded"}}}, nil
	}
	f.model.add(say("Refunded."))
	if _, err := f.r.Execute(ctx, "t1", r.ID, nil); err != nil {
		t.Fatal(err)
	}
	reqs = f.model.requests()
	if got := texts(reqs[len(reqs)-1].Messages); !slices.Equal(got, []string{"user: refund order 42"}) {
		t.Fatalf("the continued run saw %q, want only its own input", got)
	}
}

// If KIFF can't be asked, the run goes back to waiting, as it was.
func TestAFailedCollectPausesAgain(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_refund", `{}`), say("Waiting."))
	r := f.start(server.Start{Input: "refund"})
	paused := f.pause(r)
	recorded := len(f.events(r))
	f.answer(r)
	f.r.Collect = func(context.Context, agent.Agent, []run.Requirement) ([]agent.ToolResultBody, error) {
		return nil, errors.New("KIFF is down")
	}
	got, err := f.r.Execute(ctx, "t1", r.ID, nil)
	if err == nil || !strings.Contains(err.Error(), "KIFF is down") {
		t.Fatalf("got %v, want KIFF's failure", err)
	}
	if got.Status != run.Paused || len(got.Requirements) != 1 || got.Paused == nil || got.Claim != "" || got.Turns != paused.Turns {
		t.Fatalf("after a failed collect: %+v", got)
	}
	if st := f.state(r); !st.Held {
		t.Fatalf("recording %+v, want it held still", st)
	}
	if n := len(f.events(r)); n != recorded {
		t.Fatal("a failed collect recorded something")
	}
}

// A hold that expires with no alert: the sweep continues the run, which
// collects the refusal and carries on. A run that can't continue is marked
// ERROR a day later, and its recording ends.
func TestTheSweepContinuesARunWhoseHoldExpired(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_refund", `{}`), say("Waiting."))
	r := f.start(server.Start{Input: "refund"})
	f.refund.result = tool.Result{Text: "held", State: tool.StateHeld,
		Hold: &tool.Hold{Operation: "op_1", Exception: "exc_1", Expires: f.clock.now().Add(time.Hour)}}
	if _, err := f.r.Execute(ctx, "t1", r.ID, nil); err != nil {
		t.Fatal(err)
	}
	f.r.Collect = func(context.Context, agent.Agent, []run.Requirement) ([]agent.ToolResultBody, error) {
		return []agent.ToolResultBody{{CallID: "c1", Tool: "stripe_refund", Result: tool.Result{Text: "expired: refused", IsError: true}}}, nil
	}
	f.model.add(say("The refund expired unanswered."))
	sw := run.Sweeper{Runs: f.runs, Ended: f.r.Ended, Continue: func(ctx context.Context, tenant, id string) error {
		_, err := f.r.Execute(ctx, tenant, id, nil)
		return err
	}}
	if n, err := sw.Sweep(ctx, f.clock.now().Add(time.Hour), 10); err != nil || n != 0 {
		t.Fatalf("swept %d at expiry, err = %v: want the grace first", n, err)
	}
	f.clock.add(time.Hour + run.ResolvedGrace + time.Second)
	if n, err := sw.Sweep(ctx, f.clock.now(), 10); err != nil || n != 1 {
		t.Fatalf("swept %d, err = %v", n, err)
	}
	if got := f.get(r.ID); got.Status != run.Completed || got.Output != "The refund expired unanswered." {
		t.Fatalf("after the sweep: %+v", got)
	}

	gone := f.start(server.Start{Input: "refund again"})
	f.model.add(use("c2", "stripe_refund", `{}`), say("Waiting."))
	f.refund.result.Hold.Expires = f.clock.now().Add(time.Hour)
	if _, err := f.r.Execute(ctx, "t1", gone.ID, nil); err != nil {
		t.Fatal(err)
	}
	f.r.Collect = func(context.Context, agent.Agent, []run.Requirement) ([]agent.ToolResultBody, error) {
		return nil, errors.New("KIFF is down")
	}
	f.clock.add(time.Hour + run.ResolvedGrace + time.Second)
	if n, err := sw.Sweep(ctx, f.clock.now(), 10); n != 0 || err == nil {
		t.Fatalf("swept %d, err = %v: want KIFF's failure", n, err)
	}
	f.clock.add(run.PausedGiveUp)
	if n, err := sw.Sweep(ctx, f.clock.now(), 10); err != nil || n != 1 {
		t.Fatalf("swept %d, err = %v", n, err)
	}
	if got := f.get(gone.ID); got.Status != run.Error || !f.state(gone).Ended {
		t.Fatalf("given up: %s, recording %+v", got.Status, f.state(gone))
	}
}

// The sweep's backoff belongs to one pause: a continue that pauses again on
// KIFF's failure keeps it, and a new held call starts it again.
func TestANewPauseResetsTheSweepsBackoff(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_refund", `{}`), say("Waiting."))
	r := f.start(server.Start{Input: "refund"})
	f.pause(r)
	f.answer(r)
	if _, err := run.Change(ctx, f.runs, f.get(r.ID), f.clock.now(), func(n *run.Run) error {
		n.Retries, n.Retry = 3, f.clock.now().Add(8*time.Minute)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.r.Collect = func(context.Context, agent.Agent, []run.Requirement) ([]agent.ToolResultBody, error) {
		return nil, errors.New("KIFF is down")
	}
	if got, _ := f.r.Execute(ctx, "t1", r.ID, nil); got.Status != run.Paused || got.Retries != 3 {
		t.Fatalf("paused again on KIFF's failure: %s, retries %d, want 3 kept", got.Status, got.Retries)
	}
	f.r.Collect = func(context.Context, agent.Agent, []run.Requirement) ([]agent.ToolResultBody, error) {
		return []agent.ToolResultBody{{CallID: "c1", Tool: "stripe_refund", Result: tool.Result{Text: "refunded"}}}, nil
	}
	f.model.add(use("c2", "stripe_refund", `{}`), say("Waiting again."))
	f.refund.result = tool.Result{Text: "held", State: tool.StateHeld}
	got, err := f.r.Execute(ctx, "t1", r.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != run.Paused || got.Requirements[0].CallID != "c2" || got.Retries != 0 || !got.Retry.IsZero() {
		t.Fatalf("a new pause: %s on %+v, retries %d, retry %v", got.Status, got.Requirements, got.Retries, got.Retry)
	}
}

// A run asked to stop stops at its next checkpoint, after the tool batch.
func TestCancelStopsAtTheNextCheckpoint(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_refund", `{}`), say("never said"))
	r := f.start(server.Start{Input: "refund"})
	f.refund.onCall = func() {
		if _, err := run.Change(ctx, f.runs, f.get(r.ID), f.clock.now(), func(n *run.Run) error { n.Cancel = true; return nil }); err != nil {
			t.Error(err)
		}
	}
	got, err := f.r.Execute(ctx, "t1", r.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != run.Cancelled || got.Stop != string(agent.Cancelled) || got.Turns != 1 {
		t.Fatalf("cancelled to %+v", got)
	}
	if n := len(f.model.requests()); n != 1 {
		t.Fatalf("the model was called %d times after the cancel, want no more", n-1)
	}
	if !f.state(r).Ended {
		t.Fatal("a cancelled run's recording wasn't ended")
	}
}

func TestAFailedModelIsAnError(t *testing.T) {
	f := newFixture(t) // no replies: the first turn fails
	r := f.start(server.Start{Input: "go"})
	got, err := f.r.Execute(ctx, "t1", r.ID, nil)
	if err == nil || !strings.Contains(err.Error(), "no more replies") {
		t.Fatalf("got %v, want the model's failure for the caller to log", err)
	}
	if got.Status != run.Error || got.Error != runner.FailedRun || got.Expires.IsZero() {
		t.Fatalf("failed to %+v", got)
	}
	if !f.state(r).Ended || !slices.Contains(f.events(r), session.Error) {
		t.Fatalf("recording %+v with %v, want ended with the error", f.state(r), f.events(r))
	}
}

func TestAnAgentThatCantBeBuiltIsAnError(t *testing.T) {
	f := newFixture(t, say("never"))
	d := registrytest.Definition("t1", "rae")
	d.Gateway = "https://elsewhere.test/mcp"
	_ = f.agents.Put(ctx, d)
	r := f.start(server.Start{Input: "go"})
	got, err := f.r.Execute(ctx, "t1", r.ID, nil)
	if !errors.Is(err, registry.ErrGateway) || got.Status != run.Error || got.Claim != "" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if n := len(f.events(r)); n != 0 || len(f.model.requests()) != 0 {
		t.Fatal("a run that couldn't be built ran")
	}
}

// Once the sweep has ended a run, its invocation writes nothing more.
func TestALostLeaseWritesNothing(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_refund", `{}`), say("never said"))
	r := f.start(server.Start{Input: "go"})
	f.refund.onCall = func() {
		f.clock.add(runner.MaxRun + run.LeaseGrace + time.Second)
		sw := run.Sweeper{Runs: f.runs, Ended: f.r.Ended}
		if n, err := sw.Sweep(ctx, f.clock.now(), 10); n != 1 || err != nil {
			t.Errorf("swept %d, %v", n, err)
		}
	}
	began := time.Now()
	got, err := f.r.Execute(ctx, "t1", r.ID, nil)
	if !errors.Is(err, run.ErrLeaseLost) {
		t.Fatalf("got %v, want ErrLeaseLost", err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("took %v: a lost lease is final, not tried again until StopTimeout", took)
	}
	if got.Status != run.Error || !strings.Contains(got.Error, "lease ran out") {
		t.Fatalf("the run is %+v, want it as the sweep left it", got)
	}
	if n := len(f.model.requests()); n != 1 {
		t.Fatalf("the model was called %d times, want no turn after the lease ran out", n)
	}
}

func TestEndedAcceptsWhatever(t *testing.T) {
	f := newFixture(t)
	never := f.start(server.Start{Input: "go"})
	if err := f.r.Ended(ctx, never); err != nil {
		t.Fatalf("ending a run that never began: %v", err)
	}
	open := f.start(server.Start{Input: "go"})
	if err := f.store.Begin(ctx, "t1", open.Session, open.ID, open.Created); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Ended(ctx, open); err != nil || !f.state(open).Ended {
		t.Fatalf("ending an open recording: %v, %+v", err, f.state(open))
	}
	if err := f.r.Ended(ctx, open); err != nil {
		t.Fatalf("ending it again: %v", err)
	}
}

// brokenAgents is a registry whose reads fail.
type brokenAgents struct{ registry.Agents }

func (brokenAgents) Get(context.Context, string, string) (registry.Definition, error) {
	return registry.Definition{}, errors.New("throttled")
}

func TestExpiresFollowsTheAgentsKeepRules(t *testing.T) {
	f := newFixture(t)
	r := f.start(server.Start{Input: "go"})
	now := f.clock.now()
	d, err := f.agents.Get(ctx, "t1", "rae")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f.r.Expires(ctx, r, now), now.Add(d.Keep.SessionRetention()); !got.Equal(want) {
		t.Fatalf("got %v, want %v (the agent's keep rules)", got, want)
	}

	// Keep rules with a kind kept until deleted: the run is kept until deleted.
	d.Keep[session.Report] = session.Rule{Keep: session.Kept}
	if err := f.agents.Put(ctx, d); err != nil {
		t.Fatal(err)
	}
	if got := f.r.Expires(ctx, r, now); !got.IsZero() {
		t.Fatalf("got %v, want kept until deleted", got)
	}

	// An agent that can't be read: kept UnknownKeep, never for good.
	if err := f.agents.Delete(ctx, "t1", "rae"); err != nil {
		t.Fatal(err)
	}
	if got := f.r.Expires(ctx, r, now); !got.Equal(now.Add(runner.UnknownKeep)) {
		t.Fatalf("removed agent: got %v", got)
	}
	f.r.Builder.Agents = brokenAgents{}
	if got := f.r.Expires(ctx, r, now); !got.Equal(now.Add(runner.UnknownKeep)) {
		t.Fatalf("failed read: got %v", got)
	}
}

func TestANamedRunStartsOnce(t *testing.T) {
	f := newFixture(t)
	s := server.Start{Tenant: "t1", Agent: "rae", Trigger: run.BySchedule, Source: "morning", Input: "go",
		ID: "0123456789abcdef0123456789abcdef"}
	first, err := f.r.Start(ctx, s)
	if err != nil || first.ID != s.ID || first.Status != run.Pending {
		t.Fatalf("got %+v (%v)", first, err)
	}
	again, err := f.r.Start(ctx, s)
	if !errors.Is(err, run.ErrExists) || again.ID != first.ID || again.Session != first.Session {
		t.Fatalf("again: %+v (%v), want the stored run with ErrExists", again, err)
	}
	if list, _ := f.store.List(ctx, "t1", "rae", 10); len(list) != 1 {
		t.Fatalf("%d sessions: a second delivery must open none", len(list))
	}
}

// lateRuns is a run store that answers the first Get as not found, as when
// another delivery stores the run between this one's Get and its Create.
type lateRuns struct {
	run.Runs
	gets int
}

func (l *lateRuns) Get(ctx context.Context, tenant, id string) (run.Run, error) {
	if l.gets++; l.gets == 1 {
		return run.Run{}, run.ErrNotFound
	}
	return l.Runs.Get(ctx, tenant, id)
}

func TestANamedRunStartedMeanwhileIsReturned(t *testing.T) {
	f := newFixture(t)
	s := server.Start{Tenant: "t1", Agent: "rae", Trigger: run.BySchedule, Source: "morning", Input: "go",
		ID: "0123456789abcdef0123456789abcdef"}
	first, err := f.r.Start(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	f.r.Runs = &lateRuns{Runs: f.r.Runs}
	again, err := f.r.Start(ctx, s)
	if !errors.Is(err, run.ErrExists) || again.ID != first.ID || again.Session != first.Session {
		t.Fatalf("got %+v (%v), want the stored run with ErrExists", again, err)
	}
	if list, _ := f.store.List(ctx, "t1", "rae", 10); len(list) != 1 {
		t.Fatalf("%d sessions: the one this Start opened must be removed", len(list))
	}
}

func BenchmarkStartAndExecute(b *testing.B) {
	f := newFixture(b)
	b.ReportAllocs()
	for b.Loop() {
		f.model.add(say("done"))
		r, err := f.r.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "go"})
		if err != nil {
			b.Fatal(err)
		}
		if _, err := f.r.Execute(ctx, "t1", r.ID, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkContinue(b *testing.B) {
	f := newFixture(b)
	f.r.Collect = func(context.Context, agent.Agent, []run.Requirement) ([]agent.ToolResultBody, error) {
		return []agent.ToolResultBody{{CallID: "c1", Tool: "stripe_refund", Result: tool.Result{Text: "refunded"}}}, nil
	}
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		f.model.add(use("c1", "stripe_refund", `{}`), say("waiting"), say("done"))
		f.refund.result = tool.Result{State: tool.StateHeld}
		r, err := f.r.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "go"})
		if err != nil {
			b.Fatal(err)
		}
		if _, err := f.r.Execute(ctx, "t1", r.ID, nil); err != nil {
			b.Fatal(err)
		}
		if _, err := run.Change(ctx, f.runs, f.get(r.ID), f.clock.now(), func(n *run.Run) error {
			n.Requirements[0].Answered = f.clock.now()
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := f.r.Execute(ctx, "t1", r.ID, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// The server's run routes, on this Runner: a run started over HTTP streams
// to its answer and is stored completed.
func TestTheServerRunsOnIt(t *testing.T) {
	f := newFixture(t, say("Refunded."))
	srv := httptest.NewServer((&server.Server{
		Auth:   func(*http.Request) (server.Caller, error) { return server.Caller{Tenant: "t1"}, nil },
		Agents: f.agents, Sessions: f.store, Notes: f.store, Runs: f.runs, Runner: f.r, Now: f.clock.now,
	}).Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/agents/rae/runs", "application/json", strings.NewReader(`{"input":"refund order 42"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var end struct {
		ID     string     `json:"id"`
		Status run.Status `json:"status"`
		Output string     `json:"output"`
	}
	_, last, _ := strings.Cut(string(body), "event: end\ndata: ")
	if err := json.Unmarshal([]byte(strings.TrimSpace(last)), &end); err != nil {
		t.Fatalf("no end event in %s", body)
	}
	if end.Status != run.Completed || end.Output != "Refunded." || !strings.Contains(string(body), "event: delta\n") {
		t.Fatalf("streamed %s", body)
	}
	if got := f.get(end.ID); got.Status != run.Completed {
		t.Fatalf("stored %+v", got)
	}
}

// What a run's failure was caused by stays out of what the tenant is shown:
// the run's error, streamed or read, and its events replayed.
func TestTheServerShowsNoPrivateCause(t *testing.T) {
	f := newFixture(t) // no replies: a model turn fails with "script: no more replies"
	srv := httptest.NewServer((&server.Server{
		Auth:   func(*http.Request) (server.Caller, error) { return server.Caller{Tenant: "t1"}, nil },
		Agents: f.agents, Sessions: f.store, Notes: f.store, Runs: f.runs, Runner: f.r, Now: f.clock.now,
	}).Handler())
	defer srv.Close()
	post := func(path, body string) string {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	runID := func(stream string) string {
		t.Helper()
		_, last, _ := strings.Cut(stream, "event: end\ndata: ")
		var end struct {
			ID     string     `json:"id"`
			Status run.Status `json:"status"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(last)), &end); err != nil || end.Status != run.Error {
			t.Fatalf("want an end event with the run failed, in %s", stream)
		}
		return end.ID
	}

	f.connectErr = errors.New("private backend diagnostic")
	built := post("/agents/rae/runs", `{"input":"go"}`)
	if strings.Contains(built, "private backend diagnostic") || !strings.Contains(built, runner.FailedBuild) {
		t.Fatalf("a failed build streamed %s", built)
	}
	runID(built)

	f.connectErr = nil
	stream := post("/agents/rae/runs", `{"input":"go"}`)
	id := runID(stream)
	replay := post("/agents/rae/runs/"+id+"/resume", "")
	resp, err := http.Get(srv.URL + "/agents/rae/runs/" + id)
	if err != nil {
		t.Fatal(err)
	}
	read, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for name, body := range map[string]string{"streamed": stream, "replayed": replay, "read": string(read)} {
		if strings.Contains(body, "no more replies") {
			t.Errorf("the model's failure is %s: %s", name, body)
		}
	}
	if !strings.Contains(replay, agent.ModelFailed) || !strings.Contains(string(read), runner.FailedRun) {
		t.Fatalf("replayed %s\nread %s", replay, read)
	}
}

// A run asked to stop while KIFF comes to hold one of its calls pauses, and
// once KIFF has resolved the hold it records the outcome and stops, CANCELLED.
func TestACancelDuringAHeldBatchEndsOnceResolved(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_refund", `{}`), say("Waiting."))
	r := f.start(server.Start{Input: "refund"})
	f.refund.onCall = func() {
		f.refund.onCall = nil
		if _, err := run.Change(ctx, f.runs, f.get(r.ID), f.clock.now(), func(n *run.Run) error { n.Cancel = true; return nil }); err != nil {
			t.Error(err)
		}
	}
	paused := f.pause(r)
	if paused.Status != run.Paused || !paused.Cancel {
		t.Fatalf("paused %+v, want paused and asked to stop", paused)
	}
	f.answer(r)
	f.r.Collect = func(context.Context, agent.Agent, []run.Requirement) ([]agent.ToolResultBody, error) {
		return []agent.ToolResultBody{{CallID: "c1", Tool: "stripe_refund", Result: tool.Result{Text: "refused by the owner"}}}, nil
	}
	turns := len(f.model.requests())
	got, err := f.r.Execute(ctx, "t1", r.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != run.Cancelled || got.Stop != string(agent.Cancelled) || got.Requirements != nil {
		t.Fatalf("ended %+v, want cancelled", got)
	}
	if n := len(f.model.requests()); n != turns {
		t.Fatalf("the model was called %d more times, want none", n-turns)
	}
	s, _, _ := f.store.Read(ctx, "t1", r.Session)
	if !f.state(r).Ended || len(s.Held) != 0 {
		t.Fatal("the cancelled run's recording still holds its session")
	}
}

// ctxRuns refuses a call whose context is done, as a real store's client does.
type ctxRuns struct{ *memory.Runs }

func (c ctxRuns) Get(ctx context.Context, tenant, id string) (run.Run, error) {
	if err := ctx.Err(); err != nil {
		return run.Run{}, err
	}
	return c.Runs.Get(ctx, tenant, id)
}

func (c ctxRuns) Put(ctx context.Context, prev, next run.Run) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.Runs.Put(ctx, prev, next)
}

// The loop stops StopTimeout before the caller's deadline, so where the run
// stopped is stored in time. Only the loop does: the claim has the whole
// deadline, so a run is claimed and stored even with little time left.
func TestTheLoopLeavesTimeToStoreTheStop(t *testing.T) {
	f := newFixture(t, say("too late"))
	f.r.Runs = ctxRuns{f.runs}
	r := f.start(server.Start{Input: "go"})
	dctx, cancel := context.WithDeadline(ctx, time.Now().Add(runner.StopTimeout/2))
	defer cancel()
	got, err := f.r.Execute(dctx, "t1", r.ID, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the deadline", err)
	}
	if got.Status != run.Error || got.Error != runner.FailedTime || got.Claim != "" {
		t.Fatalf("stored %+v, want the time limit", got)
	}
}

// flakyRuns fails the next fail reads.
type flakyRuns struct {
	*memory.Runs
	mu   sync.Mutex
	fail int
}

func (fr *flakyRuns) Get(ctx context.Context, tenant, id string) (run.Run, error) {
	fr.mu.Lock()
	failing := fr.fail > 0
	if failing {
		fr.fail--
	}
	fr.mu.Unlock()
	if failing {
		return run.Run{}, errors.New("throttled")
	}
	return fr.Runs.Get(ctx, tenant, id)
}

// A store's passing error at a checkpoint doesn't end the run.
func TestAFailedCheckpointReadCarriesOn(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_refund", `{}`), say("Refunded."))
	flaky := &flakyRuns{Runs: f.runs}
	f.r.Runs = flaky
	f.refund.onCall = func() { flaky.mu.Lock(); flaky.fail = 1; flaky.mu.Unlock() }
	r := f.start(server.Start{Input: "refund"})
	got, err := f.r.Execute(ctx, "t1", r.ID, nil)
	if err != nil || got.Status != run.Completed {
		t.Fatalf("got %+v, %v; want it completed", got, err)
	}
}

// A run whose turn after the hold failed still pauses, and the caller gets the
// failure to log.
func TestAPauseKeepsTheCauseOfAFailedTurn(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_refund", `{}`)) // the turn after it fails
	r := f.start(server.Start{Input: "refund"})
	f.refund.result = tool.Result{Text: "held", State: tool.StateHeld}
	got, err := f.r.Execute(ctx, "t1", r.ID, nil)
	if err == nil || !strings.Contains(err.Error(), "no more replies") {
		t.Fatalf("got %v, want the failed turn's cause", err)
	}
	if got.Status != run.Paused || len(got.Requirements) != 1 {
		t.Fatalf("stored %+v, want it paused", got)
	}
}

// throttled is a run store whose writes of a given status fail a number of
// times first, as DynamoDB's client can under throttling, and whose Create can
// fail.
type throttled struct {
	*memory.Runs
	status     run.Status
	fails      int
	failCreate bool
}

func (s *throttled) Put(cx context.Context, prev, next run.Run) error {
	if next.Status == s.status && s.fails > 0 {
		s.fails--
		return errors.New("throttled")
	}
	return s.Runs.Put(cx, prev, next)
}

func (s *throttled) Create(cx context.Context, r run.Run) error {
	if s.failCreate {
		return errors.New("throttled")
	}
	return s.Runs.Create(cx, r)
}

// A write that may pass (a throttled one) is tried again when storing where
// a run stopped, so an answered or paused run isn't lost to the sweep.
func TestAPassingFailureStoringTheStopIsRetried(t *testing.T) {
	for _, c := range []struct {
		status  run.Status
		replies []model.Reply
		held    bool
	}{
		{run.Completed, []model.Reply{say("Done.")}, false},
		{run.Paused, []model.Reply{use("c1", "stripe_refund", `{}`), say("Waiting for the owner.")}, true},
	} {
		t.Run(string(c.status), func(t *testing.T) {
			f := newFixture(t, c.replies...)
			if c.held {
				f.refund.result = tool.Result{Text: "KIFF holds this call for the owner", State: tool.StateHeld}
			}
			f.r.Runs = &throttled{Runs: f.runs, status: c.status, fails: 2}
			r := f.start(server.Start{Input: "go"})
			got, err := f.r.Execute(ctx, "t1", r.ID, nil)
			if err != nil || got.Status != c.status {
				t.Fatalf("got %+v, %v", got, err)
			}
			if stored := f.get(r.ID); stored.Status != c.status || stored.Output == "" {
				t.Fatalf("stored %+v", stored)
			}
		})
	}
}

// A run whose agent is gone by the time it executes ends ERROR, and is kept
// UnknownKeep, not for good: its keep rules went with the agent.
func TestARunWhoseAgentIsGoneExpires(t *testing.T) {
	f := newFixture(t, say("never"))
	r := f.start(server.Start{Input: "go"})
	if err := f.agents.Delete(ctx, "t1", "rae"); err != nil {
		t.Fatal(err)
	}
	got, _ := f.r.Execute(ctx, "t1", r.ID, nil)
	if got.Status != run.Error || got.Error != runner.FailedBuild || !got.Expires.Equal(f.clock.now().Add(runner.UnknownKeep)) {
		t.Fatalf("got %+v", got)
	}
}

// A run that can't be stored leaves no session behind that Start opened for
// it; a session the caller gave is left as it is.
func TestAFailedStartLeavesNoSession(t *testing.T) {
	f := newFixture(t)
	given := f.start(server.Start{Input: "first"})
	f.r.Runs = &throttled{Runs: f.runs, failCreate: true}
	if _, err := f.r.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "go"}); err == nil {
		t.Fatal("no error")
	}
	if _, err := f.r.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Session: given.Session, Input: "again"}); err == nil {
		t.Fatal("no error")
	}
	sessions, err := f.store.List(ctx, "t1", "rae", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != given.Session {
		t.Fatalf("sessions %+v, want only the given one", sessions)
	}
}

// lostOnStop is a run store that answers the write of where a run stopped
// with a lost lease, as a store does once another writer moved the run on.
type lostOnStop struct{ *memory.Runs }

func (s lostOnStop) Put(cx context.Context, prev, next run.Run) error {
	if next.Status.Final() {
		return run.ErrLeaseLost
	}
	return s.Runs.Put(cx, prev, next)
}

// An error that won't pass (a lost lease) is not tried again: storing the
// stop gives up at once rather than spending StopTimeout on it.
func TestALastingFailureStoringTheStopIsNotRetried(t *testing.T) {
	f := newFixture(t, say("Done."))
	f.r.Runs = lostOnStop{f.runs}
	r := f.start(server.Start{Input: "go"})
	began := time.Now()
	_, err := f.r.Execute(ctx, "t1", r.ID, nil)
	if !errors.Is(err, run.ErrLeaseLost) {
		t.Fatalf("err = %v, want ErrLeaseLost", err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("took %v: a lost lease was tried again", took)
	}
}
