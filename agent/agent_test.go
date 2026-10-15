package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
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

// When KIFF holds a call, the agent gets one last turn without tools to say
// what it waits for, and the run stops: no retry around the hold.
func TestAHeldCallStopsTheRun(t *testing.T) {
	refund := &fakeTool{name: "stripe_refund", access: tool.Write, gateway: true,
		result: tool.Result{Text: "Held for the owner.", IsError: true, State: tool.StateHeld}}
	m := &script{replies: []model.Reply{
		use("c1", "stripe_refund", `{"charge":"ch_big"}`),
		say("The refund of ch_big waits for your answer."),
		use("c2", "stripe_refund", `{"charge":"ch_big"}`), // never reached
	}}
	_, r := start(t, session.KindRun)
	out, _, err := agent.Run(context.Background(), newAgent(m, refund), r, nil, "Work the refund queue.", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop != agent.Held || len(out.Held) != 1 || out.Held[0].CallID != "c1" || out.Text != "The refund of ch_big waits for your answer." {
		t.Fatalf("outcome = %+v", out)
	}
	if refund.calls != 1 || len(m.reqs) != 2 {
		t.Fatalf("calls %d, model turns %d", refund.calls, len(m.reqs))
	}
	if len(m.reqs[1].Tools) != 0 {
		t.Fatal("the turn after a hold offered tools")
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
