package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/chat"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
	toolmemory "github.com/kiff/agents/tool/memory"
)

// echo answers every turn with a fixed text, and keeps what it was sent.
type echo struct {
	mu   sync.Mutex
	reqs []model.Request
}

func (e *echo) Turn(_ context.Context, req model.Request, _ func(string)) (model.Reply, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reqs = append(e.reqs, req)
	return model.Reply{Blocks: []model.Block{{Kind: model.Text, Text: "Noted."}}, Stop: model.EndTurn}, nil
}

func setup(t *testing.T) (*session.Recorder, *memory.Store, *echo, agent.Agent, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	store := memory.New()
	rec, err := session.NewRecorderAt(store, session.DefaultPolicy(30*24*time.Hour), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	m := &echo{}
	a := agent.Agent{Tenant: "t1", ID: "rae", Model: m, ModelID: "test-model", MaxTokens: 1024, MaxCost: 1_000_000,
		Tools: []tool.Tool{toolmemory.New(store, "t1", "rae")}}
	return rec, store, m, a, &now
}

// A message within the window carries the last chat on; after it, a chat
// starts fresh. Each message is its own ended session.
func TestAMessageCarriesOnTheLastChatWithinTheWindow(t *testing.T) {
	rec, store, m, a, now := setup(t)
	ctx := context.Background()
	first, _, err := chat.Send(ctx, rec, store, a, "Offer store credit first.", *now, nil)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(10 * time.Minute)
	second, _, err := chat.Send(ctx, rec, store, a, "And for Acme, ask me.", *now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("two messages shared a session")
	}
	// The second turn saw the first exchange, then its own message.
	if got := len(m.reqs[1].Messages); got != 3 {
		t.Fatalf("carried %d messages, want 3", got)
	}
	if m.reqs[1].Messages[0].Blocks[0].Text != "Offer store credit first." || m.reqs[1].Messages[1].Role != model.Assistant {
		t.Fatalf("history = %+v", m.reqs[1].Messages)
	}
	_, events, err := store.Read(ctx, "t1", first.ID)
	if err != nil || len(events) == 0 {
		t.Fatalf("first session: %d events, %v", len(events), err)
	}
	if state, _, err := store.ReadRun(ctx, "t1", first.ID, events[0].Run); err != nil || !state.Ended {
		t.Fatalf("first run %+v %v", state, err)
	}

	*now = now.Add(chat.Window + time.Minute)
	if _, _, err := chat.Send(ctx, rec, store, a, "New topic.", *now, nil); err != nil {
		t.Fatal(err)
	}
	if got := len(m.reqs[2].Messages); got != 1 {
		t.Fatalf("a chat after the window carried %d messages", got)
	}
}

// The memory tool keeps notes between chats, for this agent only.
func TestTheMemoryToolKeepsNotes(t *testing.T) {
	_, store, _, _, _ := setup(t)
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

// Three messages within the window are one conversation: the third sees both
// earlier exchanges.
func TestAConversationCarriesEveryEarlierMessage(t *testing.T) {
	rec, store, m, a, now := setup(t)
	ctx := context.Background()
	for _, text := range []string{"A", "B", "C"} {
		if _, _, err := chat.Send(ctx, rec, store, a, text, *now, nil); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Minute)
	}
	msgs := m.reqs[2].Messages
	if len(msgs) != 5 || msgs[0].Blocks[0].Text != "A" || msgs[2].Blocks[0].Text != "B" || msgs[4].Blocks[0].Text != "C" {
		t.Fatalf("third request carried %+v", msgs)
	}
}

// fragileStore honours cancellation, like a networked store, and can fail
// its next EndRun once.
type fragileStore struct {
	*memory.Store
	failEnd bool
}

func (f *fragileStore) Append(ctx context.Context, tenant, id, run string, events []session.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.Store.Append(ctx, tenant, id, run, events)
}

func (f *fragileStore) EndRun(ctx context.Context, tenant, id, run string, expires time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.failEnd {
		f.failEnd = false
		return errors.New("store unavailable")
	}
	return f.Store.EndRun(ctx, tenant, id, run, expires)
}

// cancelling is a model that calls the memory tool, then has its request
// cancelled during the next turn.
type cancelling struct {
	cancel context.CancelFunc
	turns  int
}

func (c *cancelling) Turn(ctx context.Context, _ model.Request, _ func(string)) (model.Reply, error) {
	c.turns++
	if c.turns == 1 {
		return model.Reply{Blocks: []model.Block{{Kind: model.ToolUse, CallID: "c1", Tool: toolmemory.Name, Input: json.RawMessage(`{"action":"list"}`)}}, Stop: model.CallTools}, nil
	}
	c.cancel()
	return model.Reply{}, ctx.Err()
}

// A request cancelled after a tool answered still ends its run, and what the
// tool did is dropped.
func TestACancelledChatStillEndsItsRun(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	store := &fragileStore{Store: memory.New()}
	rec, err := session.NewRecorderAt(store, session.DefaultPolicy(30*24*time.Hour), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &cancelling{cancel: cancel}
	a := agent.Agent{Tenant: "t1", ID: "rae", Model: m, ModelID: "test-model", MaxTokens: 1024, MaxCost: 1_000_000,
		Tools: []tool.Tool{toolmemory.New(store, "t1", "rae")}}
	s, _, err := chat.Send(ctx, rec, store, a, "What do you remember?", now, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	got, events, err := store.Read(context.Background(), "t1", s.ID)
	if err != nil {
		t.Fatalf("session %+v %v", got, err)
	}
	if len(events) == 0 {
		t.Fatal("nothing recorded")
	}
	state, _, err := store.ReadRun(context.Background(), "t1", s.ID, events[0].Run)
	if err != nil || !state.Ended {
		t.Fatalf("run %+v %v", state, err)
	}
	for _, e := range events {
		if e.Kind == session.ToolCall || e.Kind == session.ToolResult {
			t.Fatal("a tool call or result outlived the ended run")
		}
	}
}

// If ending fails anyway, the caller gets a NotEnded it can retry.
func TestAFailedEndCanBeRetried(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	store := &fragileStore{Store: memory.New(), failEnd: true}
	rec, err := session.NewRecorderAt(store, session.DefaultPolicy(30*24*time.Hour), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	a := agent.Agent{Tenant: "t1", ID: "rae", Model: &echo{}, ModelID: "test-model", MaxTokens: 1024, MaxCost: 1_000_000}
	s, _, err := chat.Send(context.Background(), rec, store, a, "Hi.", now, nil)
	var ne *chat.NotEnded
	if !errors.As(err, &ne) {
		t.Fatalf("err = %v", err)
	}
	if state, _, _ := store.ReadRun(context.Background(), "t1", s.ID, ne.Recording.Run()); state.Ended {
		t.Fatal("the run ended before the retry")
	}
	if err := ne.Retry(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := store.ReadRun(context.Background(), "t1", s.ID, ne.Recording.Run()); !state.Ended {
		t.Fatal("the run didn't end on the retry")
	}
}
