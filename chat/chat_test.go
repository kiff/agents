package chat_test

import (
	"context"
	"encoding/json"
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
	s, _, err := store.Read(ctx, "t1", first.ID)
	if err != nil || s.Status != session.Ended {
		t.Fatalf("first session %+v %v", s, err)
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
