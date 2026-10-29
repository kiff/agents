package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
)

// search is a model reply that searched its tools (a server tool the model
// provider runs) and then called name: the call and its result come back in
// the same message, as the provider sends them.
func search(id, callID, name, input string) model.Reply {
	r := use(callID, name, input)
	r.Blocks = append([]model.Block{
		{Kind: model.Server, Data: fmt.Sprintf(`{"type":"server_tool_use","id":%q,"name":"tool_search_tool_bm25","input":{"query":"refund"}}`, id)},
		{Kind: model.Server, Data: fmt.Sprintf(`{"type":"tool_search_tool_result","tool_use_id":%q,"content":{"type":"tool_search_tool_search_result","tool_references":[]}}`, id)},
	}, r.Blocks...)
	return r
}

// paired fails t unless every server tool's result in msgs follows its call,
// and every call has its result: what the model provider requires, or it
// refuses the turn (a 400).
func paired(t *testing.T, what string, msgs []model.Message) {
	t.Helper()
	calls, results := map[string]bool{}, map[string]bool{}
	for i, m := range msgs {
		for j, b := range m.Blocks {
			if b.Kind != model.Server {
				continue
			}
			var v struct {
				ID, ToolUseID string `json:"-"`
			}
			var raw map[string]any
			_ = json.Unmarshal([]byte(b.Data), &raw)
			v.ID, _ = raw["id"].(string)
			v.ToolUseID, _ = raw["tool_use_id"].(string)
			if v.ToolUseID != "" {
				if !calls[v.ToolUseID] {
					t.Fatalf("%s: messages.%d.content.%d: a server result with no call before it (%s)", what, i, j, v.ToolUseID)
				}
				results[v.ToolUseID] = true
			} else {
				calls[v.ID] = true
			}
		}
	}
	for id := range calls {
		if !results[id] {
			t.Fatalf("%s: server call %s with no result", what, id)
		}
	}
}

// A run that searched its tools, in its paused turn and in turns before it,
// pauses on a card or a hold and continues: every turn it sends keeps each
// server call right before its result, and a run long enough that its oldest
// turns are left out never sends half a pair.
func TestServerToolPairsSurviveAPauseAndAContinue(t *testing.T) {
	for _, turns := range []int{1, 3, agent.BeforeMax} {
		t.Run(fmt.Sprintf("%d turns before", turns), func(t *testing.T) {
			store := memory.New()
			rec, err := session.NewRecorder(store, session.DefaultPolicy(30*24*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			var replies []model.Reply
			for i := range turns {
				replies = append(replies, search(fmt.Sprintf("srvtoolu_%d", i), fmt.Sprintf("r%d", i), "stripe_get", `{"charge":"ch_1"}`))
			}
			replies = append(replies, search("srvtoolu_last", "c2", "stripe_refund", `{"charge":"ch_big"}`), say("Waiting."), say("Refunded."))
			m := &script{replies: replies}
			read := &fakeTool{name: "stripe_get", access: tool.Read, gateway: true, result: tool.Result{Text: `{"id":"ch_1"}`}}
			a := newAgent(m, read, heldRefund())
			stored, s := paused(t, store, rec, a)
			stored.Requirements[0].Answer = "approved"
			r, err := rec.Resume(context.Background(), s, stored.ID)
			if err != nil {
				t.Fatal(err)
			}
			held := []agent.ToolResultBody{{CallID: "c2", Tool: "stripe_refund", Result: tool.Result{Text: `{"id":"re_1"}`}}}
			if _, _, err := agent.Continue(context.Background(), a, r, nil, stored, held, nil); err != nil {
				t.Fatal(err)
			}
			for i, req := range m.reqs {
				paired(t, fmt.Sprintf("turn %d", i+1), req.Messages)
			}
		})
	}
}

// A paused run kept before pairing, whose earlier turns hold half a pair (its
// call cut with an older turn), still continues: the half is left out of what
// the run sends, never sent alone.
func TestHalfAServerPairIsLeftOutWhenARunContinues(t *testing.T) {
	store := memory.New()
	rec, err := session.NewRecorder(store, session.DefaultPolicy(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	m := &script{replies: []model.Reply{use("r0", "stripe_get", `{}`), use("c2", "stripe_refund", `{"charge":"ch_big"}`), say("Waiting."), say("Refunded.")}}
	read := &fakeTool{name: "stripe_get", access: tool.Read, gateway: true, result: tool.Result{Text: `{}`}}
	a := newAgent(m, read, heldRefund())
	stored, s := paused(t, store, rec, a)
	if len(stored.Paused.Before) == 0 {
		t.Fatal("no earlier turn kept")
	}
	// As a store could hold it: the earlier turn opens with a result whose call was cut.
	b := &stored.Paused.Before[0]
	b.Blocks = append([]model.Block{{Kind: model.Server, Data: `{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_gone","content":{"type":"tool_search_tool_search_result","tool_references":[]}}`}}, b.Blocks...)
	stored.Requirements[0].Answer = "approved"
	r, err := rec.Resume(context.Background(), s, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	held := []agent.ToolResultBody{{CallID: "c2", Tool: "stripe_refund", Result: tool.Result{Text: `{"id":"re_1"}`}}}
	if _, _, err := agent.Continue(context.Background(), a, r, nil, stored, held, nil); err != nil {
		t.Fatal(err)
	}
	paired(t, "the continued turn", m.reqs[len(m.reqs)-1].Messages)
}
