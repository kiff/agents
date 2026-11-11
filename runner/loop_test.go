package runner_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/tool"
)

// listRead is a gateway read whose every page has more.
type listRead struct{}

func (listRead) Spec() model.ToolSpec {
	return model.ToolSpec{Name: "stripe_list", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (listRead) Access() tool.Access { return tool.Read }
func (listRead) Gateway() bool       { return true }
func (listRead) Call(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{Text: `{"data":[{"id":"dp_0042"}],"has_more":true}`}, nil
}

// paging is LoopPages turns reading the next page of the same list, then words.
func paging(last string) []model.Reply {
	out := make([]model.Reply, 0, agent.LoopPages+1)
	for i := range agent.LoopPages {
		out = append(out, use(fmt.Sprintf("c%d", i+1), "stripe_list", fmt.Sprintf(`{"limit":100,"starting_after":"dp_%04d"}`, i)))
	}
	return append(out, say(last))
}

// The runner hands the loop who asked for the run, as the app and the
// schedule start them: a member's chat run reaches its tenth page with a
// person behind it and pauses on the loop card; a scheduled run has no one,
// shows no card, and closes with its answer.
func TestTheRunnerTellsTheLoopWhetherAPersonIsBehindTheRun(t *testing.T) {
	ana := run.Requester{Kind: run.ByMember, ID: "ana"}

	f := newFixture(t, paging("Waiting for you.")...)
	f.extra = []tool.Tool{listRead{}}
	chat := f.start(server.Start{Input: "List the open disputes.", Requester: ana})
	got, err := f.r.Execute(ctx, "t1", chat.ID, nil)
	if err != nil || got.Status != run.Paused || len(got.Requirements) != 1 || !got.Requirements[0].Card() ||
		!strings.Contains(string(got.Requirements[0].Input), `"id":"`+agent.LoopCard) {
		t.Fatalf("a member's chat run: %+v, %v; want it paused on the loop card", got, err)
	}

	f = newFixture(t, paging("Read 1,000 disputes; more remain unread.")...)
	f.extra = []tool.Tool{listRead{}}
	job := f.start(server.Start{Tenant: "t1", Agent: "rae", Trigger: run.BySchedule, Input: "List the open disputes."})
	got, err = f.r.Execute(ctx, "t1", job.ID, nil)
	if err != nil || got.Status != run.Completed || got.Stop != string(agent.Answered) || len(got.Requirements) != 0 ||
		got.Output != "Read 1,000 disputes; more remain unread." {
		t.Fatalf("a scheduled run: %+v, %v; want it answered with no card", got, err)
	}
}
