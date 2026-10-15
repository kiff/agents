package chat_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/chat"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/registry/registrytest"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/runner"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
	toolmemory "github.com/kiff/agents/tool/memory"
)

// echo answers "Noted." and keeps what it was sent. A message that asks for
// a refund gets a refund call first, and the call's result is answered with
// "Waiting for your answer.".
type echo struct {
	mu   sync.Mutex
	reqs []model.Request
}

func (e *echo) Turn(_ context.Context, req model.Request, onText func(string)) (model.Reply, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reqs = append(e.reqs, req)
	last := req.Messages[len(req.Messages)-1].Blocks[0]
	if last.Kind != model.ToolResult && strings.HasPrefix(last.Text, "Refund") {
		return model.Reply{Blocks: []model.Block{{Kind: model.ToolUse, CallID: "c1", Tool: "stripe_refund", Input: json.RawMessage(`{}`)}},
			Stop: model.CallTools}, nil
	}
	text := "Noted."
	if last.Kind == model.ToolResult {
		text = "Waiting for your answer."
	}
	if onText != nil {
		onText(text)
	}
	return model.Reply{Blocks: []model.Block{{Kind: model.Text, Text: text}}, Stop: model.EndTurn}, nil
}

func (e *echo) last() model.Request {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reqs[len(e.reqs)-1]
}

// held is a refund KIFF holds for the owner.
type held struct{}

func (held) Spec() model.ToolSpec {
	return model.ToolSpec{Name: "stripe_refund", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (held) Access() tool.Access { return tool.Write }
func (held) Gateway() bool       { return true }
func (held) Call(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{Text: "Held for the owner.", IsError: true, State: tool.StateHeld}, nil
}

type keys struct{}

func (keys) Key(context.Context, string, string) (string, error) { return "key", nil }

type env struct {
	runner *runner.Runner
	runs   *memory.Runs
	store  *memory.Store
	model  *echo
}

// setup is a runner on memory stores, with t1's agents "rae" and "max".
func setup(t *testing.T) env {
	t.Helper()
	ctx := context.Background()
	agents := memory.NewAgents()
	for _, id := range []string{"rae", "max"} {
		if err := agents.Put(ctx, registrytest.Definition("t1", id)); err != nil {
			t.Fatal(err)
		}
	}
	e := env{runs: memory.NewRuns(), store: memory.New(), model: &echo{}}
	e.runner = &runner.Runner{Runs: e.runs, Sessions: e.store,
		Builder: &registry.Builder{Agents: agents, Secrets: keys{}, Model: e.model, Gateways: []string{"https://gateway.test/mcp"},
			Connect: func(context.Context, string, string) ([]tool.Tool, error) { return []tool.Tool{held{}}, nil }}}
	return e
}

func (e env) chat(agentID string) *chat.Chat {
	return &chat.Chat{Runner: e.runner, Tenant: "t1", Agent: agentID}
}

func texts(msgs []model.Message) []string {
	var out []string
	for _, m := range msgs {
		for _, b := range m.Blocks {
			out = append(out, b.Text)
		}
	}
	return out
}

// A conversation is one session, and each message a run in it, carrying the
// conversation on.
func TestEachMessageIsARunOfOneConversation(t *testing.T) {
	e := setup(t)
	c := e.chat("rae")
	ctx := context.Background()
	var made []run.Run
	for _, text := range []string{"A", "B", "C"} {
		r, err := c.Send(ctx, text, nil)
		if err != nil {
			t.Fatal(err)
		}
		made = append(made, r)
	}
	if got := texts(e.model.last().Messages); !slices.Equal(got, []string{"A", "Noted.", "B", "Noted.", "C"}) {
		t.Fatalf("the third message carried %q", got)
	}
	for _, r := range made {
		if r.Session != c.Session || r.Trigger != run.ByOwner || r.Status != run.Completed || r.Output != "Noted." {
			t.Fatalf("run %+v", r)
		}
	}
	if l, _ := e.store.List(ctx, "t1", "rae", 10); len(l) != 1 || l[0].ID != c.Session {
		t.Fatalf("%d sessions, want the one conversation", len(l))
	}
}

// A message whose call KIFF holds pauses its run. The next message is a run of
// its own, and carries nothing of the paused run until it ends.
func TestAMessageWhilePausedIsARunOfItsOwn(t *testing.T) {
	e := setup(t)
	c := e.chat("rae")
	ctx := context.Background()
	paused, err := c.Send(ctx, "Refund ch_big.", nil)
	if err != nil || paused.Status != run.Paused || len(paused.Requirements) != 1 {
		t.Fatalf("paused %+v, %v", paused, err)
	}
	next, err := c.Send(ctx, "Meanwhile, hello.", nil)
	if err != nil || next.Status != run.Completed || next.Session != paused.Session || next.ID == paused.ID {
		t.Fatalf("next %+v, %v", next, err)
	}
	if got := texts(e.model.last().Messages); !slices.Equal(got, []string{"Meanwhile, hello."}) {
		t.Fatalf("a message during a pause carried %q", got)
	}
}

// Two conversations with the same agent are apart, and so are two agents'.
func TestConversationsAreApart(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	one, two, other := e.chat("rae"), e.chat("rae"), e.chat("max")
	for _, c := range []*chat.Chat{one, two, other} {
		if _, err := c.Send(ctx, "Hi.", nil); err != nil {
			t.Fatal(err)
		}
	}
	if one.Session == two.Session || one.Session == other.Session {
		t.Fatalf("sessions %q %q %q", one.Session, two.Session, other.Session)
	}
	if _, err := two.Send(ctx, "Again.", nil); err != nil {
		t.Fatal(err)
	}
	if got := texts(e.model.last().Messages); !slices.Equal(got, []string{"Hi.", "Noted.", "Again."}) {
		t.Fatalf("the second conversation carried %q", got)
	}
}

// A message streams its run as it goes.
func TestSendStreams(t *testing.T) {
	e := setup(t)
	var kinds []agent.EventKind
	if _, err := e.chat("rae").Send(context.Background(), "Hi.", func(ev agent.Event) { kinds = append(kinds, ev.Kind) }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kinds, []agent.EventKind{agent.TextDelta}) {
		t.Fatalf("streamed %v", kinds)
	}
}

// The memory tool keeps notes between chats, for this agent only.
func TestTheMemoryToolKeepsNotes(t *testing.T) {
	store := memory.New()
	ctx := context.Background()
	mem := toolmemory.New(store, "t1", "rae")
	call := func(in string) tool.Result {
		t.Helper()
		r, err := mem.Call(ctx, json.RawMessage(in))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := call(`{"action":"write","name":"acme","text":"Ask first."}`); r.IsError {
		t.Fatal(r.Text)
	}
	if r := call(`{"action":"list"}`); r.Text != "## acme\nAsk first." {
		t.Fatalf("list = %q", r.Text)
	}
	other, _ := toolmemory.New(store, "t1", "max").Call(ctx, json.RawMessage(`{"action":"list"}`))
	if other.Text != "No notes yet." {
		t.Fatalf("another agent read %q", other.Text)
	}
	if r := call(`{"action":"forget","name":"acme"}`); r.IsError {
		t.Fatal(r.Text)
	}
	if r := call(`{"action":"write","name":""}`); !r.IsError {
		t.Fatal("a nameless note was written")
	}
	if mem.Access() != tool.Own || mem.Gateway() {
		t.Fatal("memory must be Own, outside the gateway")
	}
}
