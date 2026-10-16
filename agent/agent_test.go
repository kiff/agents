package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
)

// script is a fake model: it answers each turn with the next reply, and keeps
// the requests it was sent.
type script struct {
	mu      sync.Mutex
	replies []model.Reply
	reqs    []model.Request
}

func (s *script) Turn(_ context.Context, req model.Request, onText func(string)) (model.Reply, error) {
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

func say(text string) model.Reply {
	return model.Reply{Blocks: []model.Block{{Kind: model.Text, Text: text}}, Stop: model.EndTurn, Usage: model.Usage{InputTokens: 1000, OutputTokens: 100}}
}

func use(id, name, input string) model.Reply {
	return model.Reply{Blocks: []model.Block{{Kind: model.ToolUse, CallID: id, Tool: name, Input: json.RawMessage(input)}}, Stop: model.CallTools,
		Usage: model.Usage{InputTokens: 1000, OutputTokens: 100}}
}

// fakeTool answers every call with result, through the gateway or not.
type fakeTool struct {
	name    string
	access  tool.Access
	gateway bool
	result  tool.Result
	calls   int
	mu      sync.Mutex
}

func (f *fakeTool) Spec() model.ToolSpec {
	return model.ToolSpec{Name: f.name, InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (f *fakeTool) Access() tool.Access { return f.access }
func (f *fakeTool) Gateway() bool       { return f.gateway }
func (f *fakeTool) Call(context.Context, json.RawMessage) (tool.Result, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.result, nil
}

// price makes 1000 input and 100 output tokens cost 1000 millionths of a dollar.
var price = model.Price{Input: 500_000, Output: 5_000_000}

func newAgent(m model.Model, tools ...tool.Tool) agent.Agent {
	return agent.Agent{Tenant: "t1", ID: "rae", Model: m, ModelID: "test-model", Price: price, MaxTokens: 1024,
		System: "You refund orders.", Tools: tools, MaxCost: 1_000_000}
}

func start(t *testing.T, kind session.Kind) (*memory.Store, *session.Recording) {
	t.Helper()
	store := memory.New()
	rec, err := session.NewRecorder(store, session.DefaultPolicy(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	s, err := rec.Open(context.Background(), "t1", "rae", kind)
	if err != nil {
		t.Fatal(err)
	}
	r, err := rec.Start(context.Background(), s, "r1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return store, r
}

func kinds(events []session.Event) []session.EventKind {
	out := make([]session.EventKind, len(events))
	for i, e := range events {
		out[i] = e.Kind
	}
	return out
}

func equal[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A full turn: the model calls a tool through the gateway, reads its result
// and answers. Every step is recorded in order, streamed, and costed.
func TestAFullTurnIsRecordedAndStreamed(t *testing.T) {
	refund := &fakeTool{name: "stripe_refund", access: tool.Write, gateway: true, result: tool.Result{Text: "refunded re_1"}}
	m := &script{replies: []model.Reply{use("c1", "stripe_refund", `{"charge":"ch_1"}`), say("Refunded ch_1.")}}
	store, r := start(t, session.KindRun)
	var streamed []agent.EventKind
	out, msgs, err := agent.Run(context.Background(), newAgent(m, refund), r, nil, "Work the refund queue.", func(e agent.Event) { streamed = append(streamed, e.Kind) })
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop != agent.Answered || out.Text != "Refunded ch_1." || out.Cost != 2000 || refund.calls != 1 {
		t.Fatalf("outcome = %+v, calls %d", out, refund.calls)
	}
	if len(msgs) != 4 || msgs[3].Role != model.Assistant {
		t.Fatalf("conversation = %+v", msgs)
	}
	if err := r.End(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, events, err := store.Read(context.Background(), "t1", r.Session().ID)
	if err != nil {
		t.Fatal(err)
	}
	// The tool call and its result are dropped when the run ends (the default
	// keep rule); a run's last answer is its report.
	want := []session.EventKind{session.OwnerMessage, session.Usage, session.Usage, session.Report}
	if got := kinds(events); !equal(got, want) {
		t.Fatalf("recorded %v, want %v", got, want)
	}
	if !equal(streamed, []agent.EventKind{agent.CallStart, agent.CallResult, agent.TextDelta}) {
		t.Fatalf("streamed %v", streamed)
	}
	if cost, _ := session.Cost(events); cost != 2000 {
		t.Fatalf("recorded cost %d", cost)
	}
	if len(m.reqs[0].Tools) != 1 || m.reqs[0].System != "You refund orders." || m.reqs[0].Model != "test-model" {
		t.Fatalf("first request = %+v", m.reqs[0])
	}
}

// both is one turn calling a lookup and a refund.
func both() model.Reply {
	return model.Reply{Blocks: []model.Block{
		{Kind: model.Text, Text: "Checking, then refunding."},
		{Kind: model.ToolUse, CallID: "c1", Tool: "stripe_get", Input: json.RawMessage(`{"charge":"ch_big"}`)},
		{Kind: model.ToolUse, CallID: "c2", Tool: "stripe_refund", Input: json.RawMessage(`{"charge":"ch_big"}`)},
	}, Stop: model.CallTools, Usage: model.Usage{InputTokens: 1000, OutputTokens: 100}}
}

// holdExpires is when heldRefund's hold stops waiting.
var holdExpires = time.Date(2026, 10, 16, 9, 10, 0, 0, time.UTC)

func heldRefund() *fakeTool {
	return &fakeTool{name: "stripe_refund", access: tool.Write, gateway: true,
		result: tool.Result{Text: "Held for the owner.", IsError: true, State: tool.StateHeld,
			Hold: &tool.Hold{Operation: "op-refund-42", Exception: "exc_123", Expires: holdExpires}}}
}

func lookupTool() *fakeTool {
	return &fakeTool{name: "stripe_get", access: tool.Read, gateway: true, result: tool.Result{Text: `{"amount":90000}`}}
}

// When KIFF holds a call, the run pauses: one requirement per held call, and
// the turn it paused in, with the results of the calls KIFF didn't hold. The
// agent first gets one turn without tools to say what it waits for; it can't
// retry around the hold.
func TestAHeldCallPausesTheRun(t *testing.T) {
	refund, lookup := heldRefund(), lookupTool()
	m := &script{replies: []model.Reply{
		both(),
		say("The refund of ch_big waits for your answer."),
		use("c3", "stripe_refund", `{"charge":"ch_big"}`), // never reached
	}}
	_, r := start(t, session.KindRun)
	out, _, err := agent.Run(context.Background(), newAgent(m, lookup, refund), r, nil, "Work the refund queue.", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop != agent.Paused || out.Text != "The refund of ch_big waits for your answer." || out.Turns != 2 || out.Cost != 2000 {
		t.Fatalf("outcome = %+v", out)
	}
	if len(out.Requirements) != 1 {
		t.Fatalf("requirements = %+v", out.Requirements)
	}
	if q := out.Requirements[0]; q.CallID != "c2" || q.Tool != "stripe_refund" || string(q.Input) != `{"charge":"ch_big"}` ||
		q.Operation != "op-refund-42" || q.Exception != "exc_123" || !q.Expires.Equal(holdExpires) {
		t.Fatalf("requirement = %+v", q)
	}
	if out.Turn == nil || !reflect.DeepEqual(out.Turn.Reply, model.Message{Role: model.Assistant, Blocks: both().Blocks}) {
		t.Fatalf("paused turn = %+v", out.Turn)
	}
	if want := []model.Block{{Kind: model.ToolResult, CallID: "c1", Result: `{"amount":90000}`}}; !reflect.DeepEqual(out.Turn.Results, want) {
		t.Fatalf("kept results = %+v", out.Turn.Results)
	}
	if refund.calls != 1 || lookup.calls != 1 || len(m.reqs) != 2 {
		t.Fatalf("calls %d and %d, model turns %d", refund.calls, lookup.calls, len(m.reqs))
	}
	if say := m.reqs[1]; !say.NoToolUse || len(say.Tools) != 2 {
		t.Fatalf("the turn after a hold: NoToolUse %v, %d tools; want the same tools, use forbidden", say.NoToolUse, len(say.Tools))
	}
	if m.reqs[0].NoToolUse {
		t.Fatal("a working turn forbade tool use")
	}
}

// A run whose caps are reached in the turn KIFF holds a call still pauses,
// so the held call has a run to continue; it skips the turn that says what
// it waits for.
func TestACapNeverSkipsAPause(t *testing.T) {
	for name, limit := range map[string]func(*agent.Agent){
		"cost":  func(a *agent.Agent) { a.MaxCost = 1000 },
		"turns": func(a *agent.Agent) { a.MaxTurns = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			m := &script{replies: []model.Reply{both(), say("never")}}
			a := newAgent(m, lookupTool(), heldRefund())
			limit(&a)
			_, r := start(t, session.KindRun)
			out, _, err := agent.Run(context.Background(), a, r, nil, "Go.", nil)
			if err != nil {
				t.Fatal(err)
			}
			if out.Stop != agent.Paused || len(out.Requirements) != 1 || out.Turn == nil || len(m.reqs) != 1 {
				t.Fatalf("outcome %+v after %d turns", out, len(m.reqs))
			}
		})
	}
}

// paused runs the agent until it pauses, pauses the recording, and returns
// the run as it would be stored, and its session.
func paused(t *testing.T, store *memory.Store, rec *session.Recorder, a agent.Agent) (run.Run, session.Session) {
	t.Helper()
	ctx := context.Background()
	s, err := rec.Open(ctx, "t1", "rae", session.KindRun)
	if err != nil {
		t.Fatal(err)
	}
	r, err := rec.Start(ctx, s, "r1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := agent.Run(ctx, a, r, nil, "Work the refund queue.", nil)
	if err != nil || out.Stop != agent.Paused {
		t.Fatalf("outcome %+v, %v", out, err)
	}
	if err := r.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	return run.Run{ID: "r1", Tenant: "t1", Agent: "rae", Session: s.ID, Status: run.Running, Input: "Work the refund queue.",
		Cost: out.Cost, Turns: out.Turns, Requirements: out.Requirements, Paused: out.Turn}, s
}

func approved() []agent.ToolResultBody {
	return []agent.ToolResultBody{{CallID: "c2", Tool: "stripe_refund", Result: tool.Result{Text: "refunded re_1", State: "allowed"}}}
}

// A paused run continues from its stored turn with the held calls' outcomes:
// the model sees the input, the turn it paused in and every call's result in
// the order of the calls, not the turn that said what it waited for. Cost
// and turns carry on, and the held call's result is recorded.
func TestAPausedRunContinues(t *testing.T) {
	store := memory.New()
	rec, err := session.NewRecorder(store, session.DefaultPolicy(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	m := &script{replies: []model.Reply{both(), say("Waiting for you."), say("Refunded ch_big.")}}
	a := newAgent(m, lookupTool(), heldRefund())
	stored, s := paused(t, store, rec, a)

	ctx := context.Background()
	r, err := rec.Resume(ctx, s, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	var streamed []agent.EventKind
	out, _, err := agent.Continue(ctx, a, r, nil, stored, approved(), func(e agent.Event) { streamed = append(streamed, e.Kind) })
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop != agent.Answered || out.Text != "Refunded ch_big." || out.Turns != 3 || out.Cost != 3000 || out.Requirements != nil || out.Turn != nil {
		t.Fatalf("outcome = %+v", out)
	}
	want := []model.Message{
		{Role: model.User, Blocks: []model.Block{{Kind: model.Text, Text: "Work the refund queue."}}},
		{Role: model.Assistant, Blocks: both().Blocks},
		{Role: model.User, Blocks: []model.Block{
			{Kind: model.ToolResult, CallID: "c1", Result: `{"amount":90000}`},
			{Kind: model.ToolResult, CallID: "c2", Result: "refunded re_1"},
		}},
	}
	if got := m.reqs[2]; !reflect.DeepEqual(got.Messages, want) || len(got.Tools) != 2 {
		t.Fatalf("continued with %+v", got)
	}
	if !equal(streamed, []agent.EventKind{agent.CallResult, agent.TextDelta}) {
		t.Fatalf("streamed %v", streamed)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	_, events, err := store.ReadRun(ctx, "t1", s.ID, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-3:]
	if got := kinds(last); !equal(got, []session.EventKind{session.ToolResult, session.Usage, session.Report}) {
		t.Fatalf("recorded on continuing: %v", got)
	}
}

// Continue takes one outcome per requirement and nothing else: a missing
// one, an extra one, one KIFF still holds, or a run with no paused turn is
// refused before anything is recorded or the model is called.
func TestContinueTakesExactlyTheHeldCalls(t *testing.T) {
	store := memory.New()
	rec, err := session.NewRecorder(store, session.DefaultPolicy(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	m := &script{replies: []model.Reply{both(), say("Waiting for you.")}}
	a := newAgent(m, lookupTool(), heldRefund())
	stored, s := paused(t, store, rec, a)
	ctx := context.Background()
	r, err := rec.Resume(ctx, s, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	extra := append(approved(), agent.ToolResultBody{CallID: "c1", Tool: "stripe_get", Result: tool.Result{Text: "{}"}})
	still := []agent.ToolResultBody{{CallID: "c2", Tool: "stripe_refund", Result: tool.Result{Text: "Held.", State: tool.StateHeld}}}
	twice := append(approved(), approved()...)
	wrongTool := []agent.ToolResultBody{{CallID: "c2", Tool: "stripe_get", Result: tool.Result{Text: "refunded re_1"}}}
	noTurn := stored
	noTurn.Paused = nil
	cases := map[string]struct {
		r    run.Run
		held []agent.ToolResultBody
	}{
		"missing":   {stored, nil},
		"extra":     {stored, extra},
		"twice":     {stored, twice},
		"stillHeld": {stored, still},
		"wrongTool": {stored, wrongTool},
		"noTurn":    {noTurn, approved()},
	}
	for name, c := range cases {
		if _, _, err := agent.Continue(ctx, a, r, nil, c.r, c.held, nil); !errors.Is(err, agent.ErrResults) {
			t.Fatalf("%s: err = %v, want ErrResults", name, err)
		}
	}
	if len(m.reqs) != 2 {
		t.Fatalf("the model was called %d more times", len(m.reqs)-2)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	_, events, _ := store.ReadRun(ctx, "t1", s.ID, stored.ID)
	if events[len(events)-1].Kind != session.Report {
		t.Fatalf("something was recorded: %v", kinds(events))
	}
}

// The caps hold across pauses: a run that reached its cost cap before it
// paused stops when it continues, before any model turn.
func TestCapsHoldAcrossPauses(t *testing.T) {
	store := memory.New()
	rec, err := session.NewRecorder(store, session.DefaultPolicy(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	m := &script{replies: []model.Reply{both(), say("Waiting for you."), say("never")}}
	a := newAgent(m, lookupTool(), heldRefund())
	stored, s := paused(t, store, rec, a)
	a.MaxCost = stored.Cost
	r, err := rec.Resume(context.Background(), s, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := agent.Continue(context.Background(), a, r, nil, stored, approved(), nil)
	if err != nil || out.Stop != agent.CostCap || out.Cost != stored.Cost || len(m.reqs) != 2 {
		t.Fatalf("outcome %+v, %v, after %d turns", out, err, len(m.reqs))
	}
}

// A refused call isn't a hold: the agent reads why and carries on.
func TestARefusedCallIsReadNotAStop(t *testing.T) {
	refund := &fakeTool{name: "stripe_refund", access: tool.Write, gateway: true,
		result: tool.Result{Text: "Outside the Card.", IsError: true, State: tool.StateRefused}}
	m := &script{replies: []model.Reply{use("c1", "stripe_refund", `{}`), say("That refund is outside my Card.")}}
	_, r := start(t, session.KindChat)
	out, _, err := agent.Run(context.Background(), newAgent(m, refund), r, nil, "Refund ch_1.", nil)
	if err != nil || out.Stop != agent.Answered || len(m.reqs[1].Tools) != 1 {
		t.Fatalf("outcome %+v, %v", out, err)
	}
}

// The run stops at its cost cap, before the next model turn.
func TestTheCostCapStopsTheRun(t *testing.T) {
	lookup := &fakeTool{name: "stripe_get", access: tool.Read, gateway: true, result: tool.Result{Text: "{}"}}
	m := &script{replies: []model.Reply{use("c1", "stripe_get", `{}`), use("c2", "stripe_get", `{}`), use("c3", "stripe_get", `{}`)}}
	a := newAgent(m, lookup)
	a.MaxCost = 2000 // two turns
	_, r := start(t, session.KindRun)
	out, _, err := agent.Run(context.Background(), a, r, nil, "Look around.", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop != agent.CostCap || out.Cost != 2000 || len(m.reqs) != 2 {
		t.Fatalf("outcome %+v after %d turns", out, len(m.reqs))
	}
}

// An agent with a write tool outside KIFF's gateway doesn't start, and neither
// does one without a cost cap.
func TestAnUngovernedWriteToolIsRefused(t *testing.T) {
	direct := &fakeTool{name: "send_email", access: tool.Write, gateway: false}
	m := &script{}
	_, r := start(t, session.KindRun)
	if _, _, err := agent.Run(context.Background(), newAgent(m, direct), r, nil, "Email the customer.", nil); !errors.Is(err, tool.ErrUngoverned) {
		t.Fatalf("err = %v", err)
	}
	if len(m.reqs) != 0 || direct.calls != 0 {
		t.Fatal("an ungoverned agent ran")
	}
	// Reading, or writing only its own memory, needs no gateway.
	own := &fakeTool{name: "memory", access: tool.Own}
	read := &fakeTool{name: "docs", access: tool.Read}
	if err := newAgent(m, own, read).Check(); err != nil {
		t.Fatal(err)
	}
	noCap := newAgent(m)
	noCap.MaxCost = 0
	if err := noCap.Check(); err == nil {
		t.Fatal("an agent without a cost cap passed")
	}
}

// A call to a tool the agent doesn't have is an error the model reads.
func TestAnUnknownToolIsAnErrorResult(t *testing.T) {
	m := &script{replies: []model.Reply{use("c1", "rm_rf", `{}`), say("I can't do that.")}}
	_, r := start(t, session.KindChat)
	out, msgs, err := agent.Run(context.Background(), newAgent(m), r, nil, "Delete everything.", nil)
	if err != nil || out.Stop != agent.Answered {
		t.Fatalf("%+v %v", out, err)
	}
	if b := msgs[2].Blocks[0]; b.Kind != model.ToolResult || !b.IsError {
		t.Fatalf("tool result = %+v", b)
	}
}

// BenchmarkTurn is the loop's own overhead for one turn with one tool call,
// with an instant model and tool and the in-memory store.
func BenchmarkTurn(b *testing.B) {
	lookup := &fakeTool{name: "stripe_get", access: tool.Read, gateway: true, result: tool.Result{Text: "{}"}}
	store := memory.New()
	rec, err := session.NewRecorder(store, session.DefaultPolicy(time.Hour))
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		m := &script{replies: []model.Reply{use("c1", "stripe_get", `{}`), say("Done.")}}
		s, err := rec.Open(ctx, "t1", "rae", session.KindRun)
		if err != nil {
			b.Fatal(err)
		}
		r, err := rec.Start(ctx, s, "r1", time.Now())
		if err != nil {
			b.Fatal(err)
		}
		if _, _, err := agent.Run(ctx, newAgent(m, lookup), r, nil, "Go.", nil); err != nil {
			b.Fatal(err)
		}
		if err := r.End(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPauseAndContinue is the loop's own overhead for a run that pauses
// on a held call and continues from its stored turn, with an instant model
// and tools and the in-memory store: start, pause, resume, continue, end.
func BenchmarkPauseAndContinue(b *testing.B) {
	lookup, refund := lookupTool(), heldRefund()
	store := memory.New()
	rec, err := session.NewRecorder(store, session.DefaultPolicy(time.Hour))
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	held := approved()
	b.ReportAllocs()
	for b.Loop() {
		m := &script{replies: []model.Reply{both(), say("Waiting for you."), say("Done.")}}
		a := newAgent(m, lookup, refund)
		s, err := rec.Open(ctx, "t1", "rae", session.KindRun)
		if err != nil {
			b.Fatal(err)
		}
		r, err := rec.Start(ctx, s, "r1", time.Now())
		if err != nil {
			b.Fatal(err)
		}
		out, _, err := agent.Run(ctx, a, r, nil, "Go.", nil)
		if err != nil {
			b.Fatal(err)
		}
		if err := r.Pause(ctx); err != nil {
			b.Fatal(err)
		}
		stored := run.Run{ID: "r1", Input: "Go.", Cost: out.Cost, Turns: out.Turns, Requirements: out.Requirements, Paused: out.Turn}
		if r, err = rec.Resume(ctx, s, "r1"); err != nil {
			b.Fatal(err)
		}
		if _, _, err := agent.Continue(ctx, a, r, nil, stored, held, nil); err != nil {
			b.Fatal(err)
		}
		if err := r.End(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// A run asked to stop stops at its next checkpoint: after the tool batch,
// before the model's next turn.
func TestCancelStopsAtACheckpoint(t *testing.T) {
	m := &script{replies: []model.Reply{use("c1", "lookup", `{}`), say("never said")}}
	lookup := &fakeTool{name: "lookup", access: tool.Read, result: tool.Result{Text: "found"}}
	a := newAgent(m, lookup)
	asked := 0
	a.Cancel = func(context.Context) (bool, error) {
		asked++
		return asked > 1, nil // asked after the tool batch
	}
	_, rec := start(t, session.KindChat)
	out, _, err := agent.Run(context.Background(), a, rec, nil, "look it up", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop != agent.Cancelled || out.Turns != 1 || len(m.reqs) != 1 || lookup.calls != 1 {
		t.Fatalf("stopped %+v after %d turns and %d calls", out, len(m.reqs), lookup.calls)
	}

	a.Cancel = func(context.Context) (bool, error) { return false, errors.New("the store is down") }
	_, rec = start(t, session.KindChat)
	if _, _, err := agent.Run(context.Background(), a, rec, nil, "again", nil); err == nil || len(m.reqs) != 1 {
		t.Fatalf("a checkpoint that failed: %v, with %d model turns", err, len(m.reqs))
	}
}

// A call through KIFF's gateway is recorded under GatewayServer, and one of
// the agent's own tools under no server: evals grade where each call went.
func TestAToolCallRecordsWhereItWent(t *testing.T) {
	refund := &fakeTool{name: "stripe_refund", access: tool.Write, gateway: true, result: tool.Result{Text: "refunded re_1"}}
	notes := &fakeTool{name: "memory", access: tool.Read, result: tool.Result{Text: "no notes"}}
	turn := model.Reply{Blocks: []model.Block{
		{Kind: model.ToolUse, CallID: "c1", Tool: "memory", Input: json.RawMessage(`{"action":"list"}`)},
		{Kind: model.ToolUse, CallID: "c2", Tool: "stripe_refund", Input: json.RawMessage(`{"charge":"ch_1"}`)},
	}, Stop: model.CallTools}
	m := &script{replies: []model.Reply{turn, say("Done.")}}
	store, r := start(t, session.KindRun)
	if _, _, err := agent.Run(context.Background(), newAgent(m, refund, notes), r, nil, "Go.", nil); err != nil {
		t.Fatal(err)
	}
	_, events, err := store.Read(context.Background(), "t1", r.Session().ID) // before the run ends drops them
	if err != nil {
		t.Fatal(err)
	}
	servers := map[string]string{}
	for _, e := range events {
		if e.Kind == session.ToolCall {
			var b agent.ToolCallBody
			if err := json.Unmarshal(e.Body, &b); err != nil {
				t.Fatal(err)
			}
			servers[b.Tool] = b.Server
		}
	}
	if len(servers) != 2 || servers["stripe_refund"] != agent.GatewayServer || servers["memory"] != "" {
		t.Fatalf("recorded servers %v", servers)
	}
}
