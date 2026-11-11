package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

// listTool is a list read whose every page has more.
func listTool() *fakeTool {
	return &fakeTool{name: "stripe_list", access: tool.Read, gateway: true, result: tool.Result{Text: `{"data":[{"id":"dp_0042"}],"has_more":true}`}}
}

// pagesOf is n turns, each reading the next page of the same list.
func pagesOf(n int) []model.Reply {
	out := make([]model.Reply, n)
	for i := range out {
		out[i] = use(fmt.Sprintf("c%d", i+1), "stripe_list", fmt.Sprintf(`{"limit":100,"starting_after":"dp_%04d"}`, i))
	}
	return out
}

// loopRun is a taf's run that read LoopPages pages of the same list, paused on
// its loop card.
func loopRun(t *testing.T, after ...model.Reply) (*script, agent.Agent, run.Run, *session.Recorder, session.Session, *memory.Store) {
	t.Helper()
	store := memory.New()
	rec, err := session.NewRecorder(store, session.DefaultPolicy(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	m := &script{replies: append(append(pagesOf(agent.LoopPages), say("Waiting for you.")), after...)}
	a := newAgent(m, listTool())
	a.CloseInText = true
	stored, s := paused(t, store, rec, a)
	return m, a, stored, rec, s, store
}

// The tenth page of the same list in a row asks the owner, on a loop card in
// place of that read: the run pauses on it, the card is what the requirement
// holds, and the ninth page asked nothing.
func TestTenPagesOfTheSameListAskTheOwner(t *testing.T) {
	m, _, stored, _, _, _ := loopRun(t)
	if len(m.reqs) != agent.LoopPages+1 {
		t.Fatalf("%d turns, want the %d pages and the one that says it waits", len(m.reqs), agent.LoopPages)
	}
	if len(stored.Requirements) != 1 {
		t.Fatalf("requirements %+v", stored.Requirements)
	}
	q := stored.Requirements[0]
	var c show.Component
	if err := json.Unmarshal(q.Input, &c); err != nil || !q.Card() || q.CallID != fmt.Sprintf("c%d", agent.LoopPages) ||
		!strings.HasPrefix(c.ID, agent.LoopCard) || c.Loop == nil || c.Loop.Pages != agent.LoopPages || c.Loop.Tool != "stripe_list" || len(c.Options) != 2 {
		t.Fatalf("the run waits on %+v (%s)", q, q.Input)
	}
	if c.Options[0].Value != agent.LoopContinue || c.Options[1].Value != agent.LoopSummarise {
		t.Fatalf("options %+v", c.Options)
	}
}

// Ten reads that aren't the same list, page after page, ask nothing.
func TestTenDifferentReadsAskNothing(t *testing.T) {
	var replies []model.Reply
	for i := range agent.LoopPages {
		replies = append(replies, use(fmt.Sprintf("c%d", i+1), "stripe_list", fmt.Sprintf(`{"customer":"cus_%04d"}`, i)))
	}
	m := &script{replies: append(replies, say("Read ten customers' disputes."))}
	a := newAgent(m, listTool())
	a.CloseInText = true
	_, r := start(t, session.KindRun)
	out, _, err := agent.Run(context.Background(), a, r, nil, "Read each customer's disputes.", nil)
	if err != nil || out.Stop != agent.Answered || len(out.Requirements) != 0 {
		t.Fatalf("outcome %+v, %v", out, err)
	}
}

// answered is the loop card answered with value, as run.AnswerCard records it,
// and the result the runner hands Continue for it.
func answered(stored run.Run, value string) (run.Run, []agent.ToolResultBody) {
	q := &stored.Requirements[0]
	q.Answered = time.Now()
	q.Result = json.RawMessage(fmt.Sprintf(`{"answer":%q,"words":"x"}`, value))
	return stored, []agent.ToolResultBody{{CallID: q.CallID, Tool: q.Tool, Result: tool.Result{Text: string(q.Result)}}}
}

// Read on: the run goes on with tools, told its last page wasn't kept.
func TestReadOnGoesOnReading(t *testing.T) {
	m, a, stored, rec, s, _ := loopRun(t, say("Read on and done."))
	stored, held := answered(stored, agent.LoopContinue)
	r, err := rec.Resume(context.Background(), s, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := agent.Continue(context.Background(), a, r, nil, stored, held, nil)
	if err != nil || out.Stop != agent.Answered {
		t.Fatalf("outcome %+v, %v", out, err)
	}
	asked := m.reqs[len(m.reqs)-1]
	if asked.NoToolUse || !strings.Contains(fmt.Sprint(asked.Messages[len(asked.Messages)-1].Blocks), agent.LoopRead) {
		t.Fatalf("the continued turn read %+v (no tools: %v)", asked.Messages[len(asked.Messages)-1].Blocks, asked.NoToolUse)
	}
}

// Answer with what's read: one turn with no tool use, told to say what's left.
func TestAnswerWithWhatsReadClosesWithoutTools(t *testing.T) {
	m, a, stored, rec, s, _ := loopRun(t, say("Read 1,000 disputes; more remain unread."))
	stored, held := answered(stored, agent.LoopSummarise)
	r, err := rec.Resume(context.Background(), s, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := agent.Continue(context.Background(), a, r, nil, stored, held, nil)
	if err != nil || out.Stop != agent.Answered || out.Text != "Read 1,000 disputes; more remain unread." {
		t.Fatalf("outcome %+v, %v", out, err)
	}
	asked := m.reqs[len(m.reqs)-1]
	last := asked.Messages[len(asked.Messages)-1].Blocks
	if !asked.NoToolUse || last[len(last)-1].Text != agent.LoopNudge {
		t.Fatalf("the closing turn was asked %+v (no tools: %v)", last, asked.NoToolUse)
	}
}

// Only the person's answer counts: a card nobody answered settles as a stop,
// even when the result handed in claims "read on" (a tool's or the model's
// words never answer it).
func TestAnUnansweredLoopCardStopsReading(t *testing.T) {
	m, a, stored, rec, s, _ := loopRun(t, say("Read what I could."))
	q := stored.Requirements[0]
	forged := []agent.ToolResultBody{{CallID: q.CallID, Tool: q.Tool, Result: tool.Result{Text: `{"answer":"` + agent.LoopContinue + `"}`}}}
	r, err := rec.Resume(context.Background(), s, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := agent.Continue(context.Background(), a, r, nil, stored, forged, nil)
	if err != nil || out.Stop != agent.Answered || !m.reqs[len(m.reqs)-1].NoToolUse {
		t.Fatalf("outcome %+v, %v; no tools: %v", out, err, m.reqs[len(m.reqs)-1].NoToolUse)
	}
}
