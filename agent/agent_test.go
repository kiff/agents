package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/show"
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
	// keep rule); a run's last answer is its report. Timing a run writes no
	// event of its own: it rides on each turn's Usage (kiff/tafs#214).
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
	// That turn is told the run pauses and to report, after the results (#78);
	// the results the run keeps for Continue carry no note.
	last := m.reqs[1].Messages[len(m.reqs[1].Messages)-1]
	if n := len(last.Blocks); n != 3 || last.Blocks[0].Kind != model.ToolResult || last.Blocks[1].Kind != model.ToolResult ||
		last.Blocks[2].Kind != model.Text || last.Blocks[2].Text != agent.PauseNote {
		t.Fatalf("the turn after a hold read %+v", last)
	}
	for _, b := range out.Turn.Results {
		if b.Kind == model.Text {
			t.Fatalf("the kept results carry the note: %+v", out.Turn.Results)
		}
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

// A run that looked something up before the turn KIFF held continues knowing
// it did (kiff/tafs, the 755 device test): its earlier turns, without their
// thinking, come between its input and the paused turn. Before, the continued
// run saw only the refund it asked for, and said it had refunded without
// looking the payment up.
func TestAContinuedRunSeesWhatItDidBeforeThePause(t *testing.T) {
	store := memory.New()
	rec, err := session.NewRecorder(store, session.DefaultPolicy(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	lookup := use("c1", "stripe_get", `{"charge":"ch_big"}`)
	lookup.Blocks = append([]model.Block{{Kind: model.Thinking, Text: "Look first.", Signature: "sig"}}, lookup.Blocks...)
	refund := use("c2", "stripe_refund", `{"charge":"ch_big"}`)
	m := &script{replies: []model.Reply{lookup, refund, say("Waiting for you."), say("Refunded ch_big.")}}
	a := newAgent(m, lookupTool(), heldRefund())
	stored, s := paused(t, store, rec, a)
	want := []model.Message{
		{Role: model.Assistant, Blocks: use("c1", "stripe_get", `{"charge":"ch_big"}`).Blocks},
		{Role: model.User, Blocks: []model.Block{{Kind: model.ToolResult, CallID: "c1", Result: `{"amount":90000}`}}},
	}
	if !reflect.DeepEqual(stored.Paused.Before, want) {
		t.Fatalf("kept before the pause: %+v", stored.Paused.Before)
	}

	ctx := context.Background()
	r, err := rec.Resume(ctx, s, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := agent.Continue(ctx, a, r, nil, stored, approved(), nil); err != nil {
		t.Fatal(err)
	}
	got := m.reqs[3].Messages
	if len(got) != 5 || !reflect.DeepEqual(got[1:3], want) || got[3].Blocks[0].CallID != "c2" || got[4].Blocks[0].Result != "refunded re_1" {
		t.Fatalf("continued with %+v", got)
	}
}

// A long earlier result is kept cut, and a long run keeps only its latest
// earlier turns, so a stored run stays small.
func TestWhatAPausedRunKeepsIsBounded(t *testing.T) {
	big := lookupTool()
	big.result = tool.Result{Text: strings.Repeat("x", agent.BeforeResultMax+100)}
	var replies []model.Reply
	for i := range agent.BeforeMax {
		replies = append(replies, use(fmt.Sprintf("l%d", i), "stripe_get", `{}`))
	}
	replies = append(replies, use("c2", "stripe_refund", `{}`), say("Waiting."))
	m := &script{replies: replies}
	a := newAgent(m, big, heldRefund())
	a.MaxTurns, a.MaxCost = 100, 100_000_000
	_, r := start(t, session.KindRun)
	out, _, err := agent.Run(context.Background(), a, r, nil, "Go.", nil)
	if err != nil || out.Stop != agent.Paused {
		t.Fatalf("outcome %+v, %v", out, err)
	}
	before := out.Turn.Before
	if len(before) != agent.BeforeMax || before[0].Blocks[0].CallID != fmt.Sprintf("l%d", agent.BeforeMax/2) {
		t.Fatalf("kept %d turns, from %+v", len(before), before[0])
	}
	if res := before[1].Blocks[0].Result; len(res) > agent.BeforeResultMax+64 || !strings.HasPrefix(res, "xxx") {
		t.Fatalf("kept a result of %d bytes", len(res))
	}
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
	// Read before the run ends (that drops tool calls), once what was
	// recorded is written: the recording writes in batches, in the background.
	if err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, events, err := store.Read(context.Background(), "t1", r.Session().ID)
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

// thinking is an opaque thinking block, as a provider returns one.
var thinking = model.Block{Kind: model.Thinking, Text: "The refund is under the Card.", Signature: "sig-abc"}

// A model's thinking goes back to it in place, before the tool call it came
// with: the next turn replays the reply exactly as it came (#68).
func TestThinkingGoesBackInPlace(t *testing.T) {
	refund := &fakeTool{name: "stripe_refund", access: tool.Write, gateway: true, result: tool.Result{Text: "refunded re_1"}}
	call := use("c1", "stripe_refund", `{"charge":"ch_1"}`)
	call.Blocks = append([]model.Block{thinking}, call.Blocks...)
	m := &script{replies: []model.Reply{call, say("Refunded ch_1.")}}
	_, r := start(t, session.KindRun)
	if _, _, err := agent.Run(context.Background(), newAgent(m, refund), r, nil, "Refund ch_1.", nil); err != nil {
		t.Fatal(err)
	}
	if got := m.reqs[1].Messages[1].Blocks; len(got) != 2 || !reflect.DeepEqual(got[0], thinking) || got[1].Kind != model.ToolUse {
		t.Fatalf("the second turn replayed %+v", got)
	}
}

// A paused turn is stored as it came, thinking too. The continued run sends it
// without its thinking: a thinking block is bound to the conversation it was
// made in, and the continued run's is another (the turns before the paused
// one are left out, and the tools are listed again), so the provider would
// refuse it.
func TestAPausedTurnKeepsItsThinking(t *testing.T) {
	refund := heldRefund()
	call := use("c1", "stripe_refund", `{"charge":"ch_big"}`)
	call.Blocks = append([]model.Block{thinking}, call.Blocks...)
	m := &script{replies: []model.Reply{call, say("Waiting for your answer.")}}
	_, r := start(t, session.KindRun)
	out, _, err := agent.Run(context.Background(), newAgent(m, refund), r, nil, "Refund ch_big.", nil)
	if err != nil || out.Stop != agent.Paused {
		t.Fatalf("outcome %+v, %v", out, err)
	}
	b, _ := json.Marshal(out.Turn) // as the run store keeps it
	var kept run.Turn
	if err := json.Unmarshal(b, &kept); err != nil || !reflect.DeepEqual(kept.Reply.Blocks[0], thinking) {
		t.Fatalf("the paused turn kept %+v, %v", kept.Reply.Blocks, err)
	}
}

func TestAContinuedRunSendsNoThinkingFromBeforeThePause(t *testing.T) {
	store := memory.New()
	rec, err := session.NewRecorder(store, session.DefaultPolicy(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	call := both()
	call.Blocks = append([]model.Block{thinking, {Kind: model.RedactedThinking, Data: "opaque"}}, call.Blocks...)
	m := &script{replies: []model.Reply{call, say("Waiting for you."), say("Refunded ch_big.")}}
	a := newAgent(m, lookupTool(), heldRefund())
	stored, s := paused(t, store, rec, a)
	if stored.Paused.Reply.Blocks[0].Kind != model.Thinking {
		t.Fatalf("the stored turn lost its thinking: %+v", stored.Paused.Reply.Blocks)
	}
	ctx := context.Background()
	r, err := rec.Resume(ctx, s, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := agent.Continue(ctx, a, r, nil, stored, approved(), nil); err != nil {
		t.Fatal(err)
	}
	sent := m.reqs[len(m.reqs)-1].Messages[1].Blocks
	for _, b := range sent {
		if b.Kind == model.Thinking || b.Kind == model.RedactedThinking {
			t.Fatalf("the continued run sent the paused turn's thinking: %+v", sent)
		}
	}
	if len(sent) != 3 || sent[0].Kind != model.Text || sent[1].CallID != "c1" || sent[2].CallID != "c2" {
		t.Fatalf("the paused turn as sent: %+v", sent)
	}
}

// A turn a safety classifier refused stops the run as refused, with the
// category it was refused for (#68).
func TestARefusedTurnStopsTheRun(t *testing.T) {
	m := &script{replies: []model.Reply{{Stop: model.Refused, Refusal: "cyber", Usage: model.Usage{InputTokens: 1000}}}}
	_, r := start(t, session.KindRun)
	out, _, err := agent.Run(context.Background(), newAgent(m), r, nil, "x", nil)
	if err != nil || out.Stop != agent.Refused || out.Refusal != "cyber" {
		t.Fatalf("outcome %+v, %v", out, err)
	}
}

// The agent's effort goes on every turn.
func TestEffortGoesOnEveryTurn(t *testing.T) {
	m := &script{replies: []model.Reply{say("Hi.")}}
	_, r := start(t, session.KindRun)
	a := newAgent(m)
	a.Effort = "high"
	if _, _, err := agent.Run(context.Background(), a, r, nil, "x", nil); err != nil {
		t.Fatal(err)
	}
	if m.reqs[0].Effort != "high" {
		t.Fatalf("effort %q", m.reqs[0].Effort)
	}
}

// The agent asks for the cache only when its price counts cache tokens:
// otherwise its cost cap would count them as free (#77).
func TestTheCacheIsAskedForOnlyWhenPriced(t *testing.T) {
	for _, c := range []struct {
		price model.Price
		want  bool
	}{
		{model.Price{Input: 1, Output: 1}, false},
		{model.Price{Input: 1, Output: 1, CacheRead: 1}, false},
		{model.Price{Input: 1, Output: 1, CacheRead: 1, CacheWrite: 1}, true},
	} {
		m := &script{replies: []model.Reply{say("Hi.")}}
		_, r := start(t, session.KindRun)
		a := newAgent(m)
		a.Price = c.price
		if _, _, err := agent.Run(context.Background(), a, r, nil, "x", nil); err != nil {
			t.Fatal(err)
		}
		if m.reqs[0].Cache != c.want {
			t.Errorf("price %+v: cache %v, want %v", c.price, m.reqs[0].Cache, c.want)
		}
	}
}

// A refusal that comes after part of the turn streamed discards that part:
// its text is no report and no answer, and a tool call in it is never made.
// The turn's usage still counts (#68).
func TestARefusalDiscardsWhatStreamedBeforeIt(t *testing.T) {
	refund := &fakeTool{name: "stripe_refund", access: tool.Write, gateway: true, result: tool.Result{Text: "refunded re_1"}}
	partial := model.Reply{Blocks: []model.Block{
		{Kind: model.Text, Text: "Here is how to"},
		{Kind: model.ToolUse, CallID: "c1", Tool: "stripe_refund", Input: json.RawMessage(`{"charge":"ch_1"}`)},
	}, Stop: model.Refused, Refusal: "cyber", Usage: model.Usage{InputTokens: 1000, OutputTokens: 100}}
	m := &script{replies: []model.Reply{partial}}
	store, r := start(t, session.KindRun)
	out, msgs, err := agent.Run(context.Background(), newAgent(m, refund), r, nil, "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop != agent.Refused || out.Refusal != "cyber" || out.Text != "" || refund.calls != 0 || out.Cost == 0 {
		t.Fatalf("outcome %+v, tool calls %d", out, refund.calls)
	}
	if len(msgs) != 1 { // the owner's message only: nothing of the refused turn
		t.Fatalf("conversation %+v", msgs)
	}
	if err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, events, _ := store.Read(context.Background(), "t1", r.Session().ID)
	for _, e := range events {
		if e.Kind == session.Report || e.Kind == session.AgentMessage || e.Kind == session.ToolCall {
			t.Fatalf("recorded %s from a refused turn", e.Kind)
		}
	}
}

// windowed is a script that knows its model's context window.
type windowed struct{ *script }

func (windowed) Window(context.Context, string) (int64, error) { return 1_000_000, nil }

// Each turn's usage says what its input was made of, and the window it had
// (#77): the second turn's messages hold the tool call and its result.
func TestEachTurnRecordsWhatItsInputWasMadeOf(t *testing.T) {
	refund := &fakeTool{name: "stripe_refund", access: tool.Write, gateway: true, result: tool.Result{Text: "refunded re_1"}}
	m := windowed{&script{replies: []model.Reply{use("c1", "stripe_refund", `{"charge":"ch_1"}`), say("Refunded ch_1.")}}}
	store, r := start(t, session.KindRun)
	if _, _, err := agent.Run(context.Background(), newAgent(m, refund), r, nil, "Work the refund queue.", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.End(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, events, err := store.Read(context.Background(), "t1", r.Session().ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []session.ContextBody
	for _, e := range events {
		var u session.UsageBody
		if e.Kind == session.Usage && json.Unmarshal(e.Body, &u) == nil && u.Context != nil {
			got = append(got, *u.Context)
		}
	}
	size := m.reqs[1].Size()
	if len(got) != 2 || got[0].Messages != 1 || got[1].Messages != 3 || got[1].WindowTokens != 1_000_000 ||
		got[1].SystemBytes != len("You refund orders.") || got[1].ToolsBytes == 0 || got[1].MessagesBytes != size.Messages ||
		got[1].ToolResultsBytes != len("refunded re_1") || got[0].ToolResultsBytes != 0 {
		t.Fatalf("context %+v", got)
	}
}

// A component the agent shows is recorded as its own event, kept like the
// agent's messages after the run ends (the call that showed it is not), and
// streamed. A refused component records nothing.
func TestAShownComponentIsKept(t *testing.T) {
	m := &script{replies: []model.Reply{
		use("c1", "show", `{"type":"notice","id":"n1","tone":"info","text":"Asking planque."}`),
		use("c2", "show", `{"type":"notice","id":"n2","tone":"loud","text":"x"}`),
		say("Done."),
	}}
	store, r := start(t, session.KindChat)
	var streamed []agent.Event
	if _, _, err := agent.Run(context.Background(), newAgent(m, show.New()), r, nil, "Connect Stripe.", func(e agent.Event) { streamed = append(streamed, e) }); err != nil {
		t.Fatal(err)
	}
	if err := r.End(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, events, err := store.Read(context.Background(), "t1", r.Session().ID)
	if err != nil {
		t.Fatal(err)
	}
	var shown []string
	for _, e := range events {
		if e.Kind == session.Component {
			shown = append(shown, string(e.Body))
		}
	}
	if len(shown) != 1 || !strings.Contains(shown[0], `"id":"n1"`) || !strings.Contains(shown[0], `"text":"Asking planque."`) {
		t.Fatalf("kept components %v, want only n1", shown)
	}
	for _, e := range events {
		if e.Kind == session.ToolCall || e.Kind == session.ToolResult {
			t.Fatalf("the show call outlived its run: %s", e.Kind)
		}
	}
	var streamedShown int
	for _, e := range streamed {
		if e.Kind == agent.Shown {
			streamedShown++
			if !strings.Contains(string(e.Component), `"id":"n1"`) {
				t.Fatalf("streamed %s", e.Component)
			}
		}
	}
	if streamedShown != 1 {
		t.Fatalf("streamed %d components", streamedShown)
	}
}

// Only a tool that shows components can put one in the owner's chat: a
// component from any other tool's result is not recorded or streamed.
func TestOnlyShowPutsAComponentInTheChat(t *testing.T) {
	other := &fakeTool{name: "lookup", access: tool.Read, result: tool.Result{Text: "ok",
		Component: json.RawMessage(`{"type":"notice","id":"n1","tone":"info","text":"from lookup"}`)}}
	m := &script{replies: []model.Reply{use("c1", "lookup", `{}`), say("Done.")}}
	store, r := start(t, session.KindChat)
	var streamed int
	if _, _, err := agent.Run(context.Background(), newAgent(m, other), r, nil, "Look it up.", func(e agent.Event) {
		if e.Kind == agent.Shown {
			streamed++
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.End(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, events, _ := store.Read(context.Background(), "t1", r.Session().ID)
	for _, e := range events {
		if e.Kind == session.Component {
			t.Fatal("a component from a tool that doesn't show was recorded")
		}
	}
	if streamed != 0 {
		t.Fatal("a component from a tool that doesn't show was streamed")
	}
}

// hookTool is a fakeTool that does something first, as the owner writing to
// the run while one of its tools runs.
type hookTool struct {
	*fakeTool
	before func()
}

func (h hookTool) Call(ctx context.Context, in json.RawMessage) (tool.Result, error) {
	h.before()
	return h.fakeTool.Call(ctx, in)
}

// only is the kind of each event that is one of of, in order.
func only(events []session.Event, of ...session.EventKind) []session.EventKind {
	var out []session.EventKind
	for _, e := range events {
		if slices.Contains(of, e.Kind) {
			out = append(out, e.Kind)
		}
	}
	return out
}

// A message the owner wrote while the run went is taken at the next
// checkpoint: recorded as an owner message, streamed, and read by the model
// after the tool batch's results, in that same user turn (#102).
func TestAnOwnersMessageIsTakenAtTheNextCheckpoint(t *testing.T) {
	m := &script{replies: []model.Reply{use("c1", "lookup", `{}`), say("Only the €20 one, then.")}}
	var waiting []string
	lookup := hookTool{
		fakeTool: &fakeTool{name: "lookup", access: tool.Read, result: tool.Result{Text: "found two"}},
		before:   func() { waiting = []string{"only the €20 one"} },
	}
	a := newAgent(m, lookup)
	a.Steer = func(context.Context) ([]string, error) {
		took := waiting
		waiting = nil
		return took, nil
	}
	var steered []string
	store, rec := start(t, session.KindChat)
	out, _, err := agent.Run(context.Background(), a, rec, nil, "refund order 12", func(e agent.Event) {
		if e.Kind == agent.Steered {
			steered = append(steered, e.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop != agent.Answered || len(m.reqs) != 2 {
		t.Fatalf("stopped %+v after %d turns", out, len(m.reqs))
	}
	if n := len(m.reqs[0].Messages); n != 1 {
		t.Fatalf("the first turn read %d messages, want the input alone", n)
	}
	last := m.reqs[1].Messages[len(m.reqs[1].Messages)-1]
	if last.Role != model.User || len(last.Blocks) != 2 || last.Blocks[0].Kind != model.ToolResult ||
		last.Blocks[1].Text != agent.SteerNote+"only the €20 one" {
		t.Fatalf("the model read %+v, want the results, then the owner's message", last)
	}
	if !equal(steered, []string{"only the €20 one"}) {
		t.Fatalf("streamed %q", steered)
	}
	if err := rec.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, events, err := store.Read(context.Background(), "t1", rec.Session().ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []session.EventKind{session.OwnerMessage, session.ToolCall, session.ToolResult, session.OwnerMessage, session.AgentMessage}
	if got := only(events, want...); !equal(got, want) {
		t.Fatalf("recorded %v, want %v", got, want)
	}

	// A checkpoint that fails stops the run, as Cancel's does.
	a.Steer = func(context.Context) ([]string, error) { return nil, errors.New("the claim is gone") }
	_, rec = start(t, session.KindChat)
	if _, _, err := agent.Run(context.Background(), a, rec, nil, "again", nil); err == nil {
		t.Fatal("a failed steer checkpoint didn't stop the run")
	}
}

// A message that came while the agent wrote its answer gets a turn of its
// own, through More, which the runner asks last; with no turn left, or none
// waiting, More returns the answer as it was (the message stays not taken,
// for the caller).
func TestAMessageDuringTheAnswerGetsATurn(t *testing.T) {
	m := &script{replies: []model.Reply{say("Refunded."), say("It was €20.")}}
	a := newAgent(m)
	var waiting []string
	a.Steer = func(context.Context) ([]string, error) {
		took := waiting
		waiting = nil
		return took, nil
	}
	_, rec := start(t, session.KindChat)
	out, msgs, err := agent.Run(context.Background(), a, rec, nil, "refund order 12", nil)
	if err != nil || out.Stop != agent.Answered || len(m.reqs) != 1 {
		t.Fatalf("answered %+v, %v, after %d turns", out, err, len(m.reqs))
	}
	waiting = []string{"how much was it?"} // came while it answered
	out, _, err = agent.More(context.Background(), a, rec, msgs, out, nil)
	if err != nil || out.Stop != agent.Answered || out.Text != "It was €20." || out.Turns != 2 || len(m.reqs) != 2 {
		t.Fatalf("after More: %+v, %v, %d turns", out, err, len(m.reqs))
	}
	second := m.reqs[1].Messages
	if len(second) != 3 || second[1].Role != model.Assistant || second[2].Role != model.User ||
		second[2].Blocks[0].Text != agent.SteerNote+"how much was it?" {
		t.Fatalf("the second turn read %+v", second)
	}
	same, _, err := agent.More(context.Background(), a, rec, msgs, out, nil)
	if err != nil || same.Turns != out.Turns {
		t.Fatalf("with nothing waiting, More ran: %+v, %v", same, err)
	}

	a.MaxTurns = 2
	asked := false
	a.Steer = func(context.Context) ([]string, error) { asked = true; return []string{"one more"}, nil }
	if same, _, err = agent.More(context.Background(), a, rec, msgs, out, nil); err != nil || same.Turns != 2 || asked {
		t.Fatalf("at the turn cap: %+v, %v, asked %v, want the answer as it was, nothing taken", same, err, asked)
	}
}

// A paused run continues knowing what the owner wrote before it paused: it
// follows the input, as the turns between them aren't carried on.
func TestAContinuedRunKnowsWhatTheOwnerWrote(t *testing.T) {
	store := memory.New()
	rec, err := session.NewRecorder(store, session.DefaultPolicy(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	m := &script{replies: []model.Reply{both(), say("Waiting for you."), say("Refunded ch_big.")}}
	a := newAgent(m, lookupTool(), heldRefund())
	stored, s := paused(t, store, rec, a)
	stored.Steering = []run.Steering{{Text: "only ch_big", Taken: true}, {Text: "not yet", Taken: false}}
	r, err := rec.Resume(context.Background(), s, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := agent.Continue(context.Background(), a, r, nil, stored, approved(), nil); err != nil {
		t.Fatal(err)
	}
	first := m.reqs[2].Messages[0]
	if first.Role != model.User || len(first.Blocks) != 2 || first.Blocks[0].Text != stored.Input ||
		first.Blocks[1].Text != agent.SteerNote+"only ch_big" {
		t.Fatalf("the continued run read %+v first, want the input and the message it took", first)
	}
}

// A gateway call that KIFF names its key and resource for leaves a call_ref,
// kept after the run like the agent's messages while the call's own events
// go (kiff/tafs#167): the Record names the payment from it. Only the
// resource's value is kept, nothing else from the input; a call KIFF names no
// resource for, or another tool's, leaves none.
func TestAGatewayCallLeavesItsReference(t *testing.T) {
	refund := &fakeTool{name: "refund", access: tool.Write, gateway: true,
		result: tool.Result{Text: "refunded re_1", Invocation: "tc-88", Resource: "pi_9"}}
	unnamed := &fakeTool{name: "lookup", access: tool.Read, gateway: true, result: tool.Result{Text: "ok", Invocation: "tc-89"}}
	m := &script{replies: []model.Reply{
		use("c1", "refund", `{"payment":"pi_9","amount":15000,"reason":"damaged"}`),
		use("c2", "lookup", `{"q":"x"}`),
		say("Done."),
	}}
	store, r := start(t, session.KindRun)
	if _, _, err := agent.Run(context.Background(), newAgent(m, refund, unnamed), r, nil, "Refund pi_9.", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.End(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, events, err := store.Read(context.Background(), "t1", r.Session().ID)
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, e := range events {
		switch e.Kind {
		case session.CallRef:
			refs = append(refs, string(e.Body))
		case session.ToolCall, session.ToolResult:
			t.Fatalf("the call outlived its run: %s", e.Kind)
		}
	}
	if len(refs) != 1 || refs[0] != `{"invocation":"tc-88","value":"pi_9"}` {
		t.Fatalf("call refs %v", refs)
	}
}

// A tool whose name starts with one of Deferred goes to the model deferred,
// and the others as before (kiff/tafs#206).
func TestDeferredToolsAreSentDeferred(t *testing.T) {
	state := &fakeTool{name: "tafs_state", access: tool.Read, result: tool.Result{Text: "{}"}}
	status := &fakeTool{name: "kiff_status", access: tool.Read, result: tool.Result{Text: "ok"}}
	m := &script{replies: []model.Reply{say("Nothing waits.")}}
	_, r := start(t, session.KindRun)
	a := newAgent(m, status, state)
	a.Deferred = []string{"tafs_"}
	if _, _, err := agent.Run(context.Background(), a, r, nil, "What waits?", nil); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range m.reqs[0].Tools {
		got[s.Name] = s.Defer
	}
	if !got["tafs_state"] || got["kiff_status"] || len(got) != 2 {
		t.Fatalf("deferred %v", got)
	}
}

// With DeferVendor, every gateway tool that isn't KIFF's own goes deferred,
// whatever its name: a vendor's tools a Card adds mid-conversation then leave
// the cached prefix as it was (#197). KIFF's own and the agent's own stay.
func TestVendorToolsAreSentDeferred(t *testing.T) {
	card := &fakeTool{name: "kiff_card", access: tool.Read, gateway: true, result: tool.Result{Text: "{}"}}
	charges := &fakeTool{name: "list_charges_2coa", access: tool.Read, gateway: true, result: tool.Result{Text: "[]"}}
	own := &fakeTool{name: "planque", access: tool.Read, result: tool.Result{Text: "ok"}}
	sent := func(deferVendor bool) map[string]bool {
		m := &script{replies: []model.Reply{say("Nothing waits.")}}
		_, r := start(t, session.KindRun)
		a := newAgent(m, card, charges, own)
		a.DeferVendor = deferVendor
		if _, _, err := agent.Run(context.Background(), a, r, nil, "What waits?", nil); err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, s := range m.reqs[0].Tools {
			got[s.Name] = s.Defer
		}
		return got
	}
	if got := sent(true); !got["list_charges_2coa"] || got["kiff_card"] || got["planque"] || len(got) != 3 {
		t.Fatalf("deferred %v", got)
	}
	if got := sent(false); got["list_charges_2coa"] {
		t.Fatalf("without DeferVendor, deferred %v", got)
	}
}

// Every turn of a run names where its history ends, so the provider caches
// that prefix apart: the next run renders the same history and reads it back,
// though it drops this run's tool calls (kiff/tafs#214, item 2).
func TestEachTurnNamesWhereTheHistoryEnds(t *testing.T) {
	refund := &fakeTool{name: "stripe_refund", access: tool.Write, gateway: true, result: tool.Result{Text: "refunded re_1"}}
	m := &script{replies: []model.Reply{use("c1", "stripe_refund", `{"charge":"ch_1"}`), say("Refunded ch_1.")}}
	_, r := start(t, session.KindRun)
	history := []model.Message{
		{Role: model.User, Blocks: []model.Block{{Kind: model.Text, Text: "Earlier."}}},
		{Role: model.Assistant, Blocks: []model.Block{{Kind: model.Text, Text: "Done earlier."}}},
	}
	if _, _, err := agent.Run(context.Background(), newAgent(m, refund), r, history, "Work the refund queue.", nil); err != nil {
		t.Fatal(err)
	}
	if len(m.reqs) != 2 {
		t.Fatalf("turns: %d", len(m.reqs))
	}
	for i, req := range m.reqs {
		if req.HistoryEnd != len(history) {
			t.Errorf("turn %d: history ends at %d, want %d", i+1, req.HistoryEnd, len(history))
		}
	}
}

// Each model turn records how long it took on its Usage, with sizes and
// never content: its tool calls and tool input, and the tool calls of the
// turn before, each with its time, result size and how many calls ran beside
// it (kiff/tafs#214, item 2). It writes no event of its own.
func TestTimingsAreRecordedWithSizesOnly(t *testing.T) {
	lookup := &fakeTool{name: "stripe_charge", access: tool.Read, gateway: true, result: tool.Result{Text: "ch_1: 30 EUR"}}
	refund := &fakeTool{name: "stripe_refund", access: tool.Write, gateway: true, result: tool.Result{Text: "refunded re_1"}}
	two := model.Reply{Blocks: []model.Block{
		{Kind: model.ToolUse, CallID: "c1", Tool: "stripe_charge", Input: json.RawMessage(`{"charge":"ch_1"}`)},
		{Kind: model.ToolUse, CallID: "c2", Tool: "stripe_refund", Input: json.RawMessage(`{"charge":"ch_1"}`)},
	}, Stop: model.CallTools, Usage: model.Usage{InputTokens: 1000, OutputTokens: 100}, ThinkingBytes: 40}
	m := &script{replies: []model.Reply{two, say("Refunded ch_1.")}}
	store, r := start(t, session.KindRun)
	a := newAgent(m, lookup, refund)
	a.Started = 1500 * time.Millisecond
	if _, _, err := agent.Run(context.Background(), a, r, nil, "Refund ch_1.", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.End(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, events, err := store.Read(context.Background(), "t1", r.Session().ID)
	if err != nil {
		t.Fatal(err)
	}
	var turns []session.TurnTiming
	for _, e := range events {
		if e.Kind != session.Usage {
			continue
		}
		var u session.UsageBody
		if err := json.Unmarshal(e.Body, &u); err != nil || u.Timing == nil {
			t.Fatalf("usage %s: %v", e.Body, err)
		}
		if strings.Contains(string(e.Body), "ch_1") || strings.Contains(string(e.Body), "EUR") {
			t.Fatalf("a timing holds content: %s", e.Body)
		}
		turns = append(turns, *u.Timing)
	}
	if len(turns) != 2 {
		t.Fatalf("timings: %+v", turns)
	}
	if s := turns[0]; s.ToolCalls != 2 || s.ToolInputBytes != 2*len(`{"charge":"ch_1"}`) || s.ThinkingBytes != 40 || s.StartMS != 1500 || len(s.Waited) != 0 {
		t.Errorf("the first turn: %+v", s)
	}
	s := turns[1]
	if s.TextBytes != len("Refunded ch_1.") || s.ToolCalls != 0 || s.StartMS != 0 || len(s.Waited) != 2 {
		t.Fatalf("the answer: %+v", s)
	}
	for i, want := range []string{"stripe_charge", "stripe_refund"} {
		if w := s.Waited[i]; w.Tool != want || w.Batch != 2 || w.ResultBytes == 0 {
			t.Errorf("a tool call: %+v", w)
		}
	}
}

// A fast agent asks for fast turns and pays the fast price for those the
// provider ran fast. Once a turn couldn't run fast, the rest of the run asks
// for standard, since the provider's cache is per speed. Each turn records
// its speed.
func TestAFastAgentKeepsOneSpeed(t *testing.T) {
	lookup := &fakeTool{name: "stripe_get_charge", access: tool.Read, result: tool.Result{Text: "ch_1: €40"}}
	fast := func(r model.Reply) model.Reply { r.Fast = true; return r }
	m := &script{replies: []model.Reply{
		fast(use("c1", "stripe_get_charge", `{"charge":"ch_1"}`)),
		use("c2", "stripe_get_charge", `{"charge":"ch_1"}`), // turned away fast: ran at standard
		say("ch_1 is €40."),
	}}
	store, r := start(t, session.KindChat)
	a := newAgent(m, lookup)
	a.Fast, a.PriceFast = true, model.Price{Input: 2 * price.Input, Output: 2 * price.Output}
	out, _, err := agent.Run(context.Background(), a, r, nil, "How much was ch_1?", func(agent.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if asked := []bool{m.reqs[0].Fast, m.reqs[1].Fast, m.reqs[2].Fast}; !equal(asked, []bool{true, true, false}) {
		t.Fatalf("asked fast %v", asked)
	}
	if out.Cost != 2000+1000+1000 {
		t.Fatalf("cost %d: the fast turn at the fast price, the others at standard", out.Cost)
	}
	if err := r.Flush(context.Background()); err != nil { // recorded in the background
		t.Fatal(err)
	}
	_, events, err := store.Read(context.Background(), "t1", r.Session().ID)
	if err != nil {
		t.Fatal(err)
	}
	var speeds []string
	for _, e := range events {
		var u session.UsageBody
		if e.Kind == session.Usage && json.Unmarshal(e.Body, &u) == nil {
			speeds = append(speeds, u.Speed)
		}
	}
	if !equal(speeds, []string{session.SpeedFast, session.SpeedStandard, session.SpeedStandard}) {
		t.Fatalf("speeds %v", speeds)
	}
	if session.SpeedOf(events) != session.SpeedStandard {
		t.Fatal("the session's speed isn't its last turn's")
	}

	// An agent that isn't fast never asks, and records no speed.
	m = &script{replies: []model.Reply{say("Hello.")}}
	store, r = start(t, session.KindChat)
	if _, _, err := agent.Run(context.Background(), newAgent(m), r, nil, "Hi", func(agent.Event) {}); err != nil {
		t.Fatal(err)
	}
	_ = r.Flush(context.Background())
	_, events, _ = store.Read(context.Background(), "t1", r.Session().ID)
	if m.reqs[0].Fast || strings.Contains(string(events[len(events)-2].Body), "speed") {
		t.Fatalf("a standard agent asked fast %v, recorded %s", m.reqs[0].Fast, events[len(events)-2].Body)
	}
}
