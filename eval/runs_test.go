package eval_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/eval"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
)

// lookup calls stripe_get, then answers.
type lookup struct{ turns int }

func (l *lookup) Turn(context.Context, model.Request, func(string)) (model.Reply, error) {
	if l.turns++; l.turns%2 == 1 {
		return model.Reply{Blocks: []model.Block{{Kind: model.ToolUse, CallID: "c1", Tool: "stripe_get", Input: json.RawMessage(`{"charge":"ch_1"}`)}},
			Stop: model.CallTools}, nil
	}
	return model.Reply{Blocks: []model.Block{{Kind: model.Text, Text: "ch_1 is 42 EUR."}}, Stop: model.EndTurn}, nil
}

type stripeGet struct{}

func (stripeGet) Spec() model.ToolSpec {
	return model.ToolSpec{Name: "stripe_get", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (stripeGet) Access() tool.Access { return tool.Read }
func (stripeGet) Gateway() bool       { return true }
func (stripeGet) Call(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{Text: `{"amount":4200}`}, nil
}

// ran records one finished run of an agent that looks a charge up, under
// policy, in session s (a new one if empty), and returns it as stored.
func ran(t *testing.T, store *memory.Store, runs run.Runs, policy session.Policy, s session.Session) (run.Run, session.Session) {
	t.Helper()
	ctx := context.Background()
	rec, err := session.NewRecorder(store, policy)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID == "" {
		if s, err = rec.Open(ctx, "t1", "rae", session.KindRun); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	r := run.New("t1", "rae", s.ID, run.ByEval, "Look up ch_1.", now)
	if err := runs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	if r, err = run.Claim(ctx, runs, "t1", r.ID, "i1", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	recording, err := rec.Start(ctx, s, r.ID, r.Created)
	if err != nil {
		t.Fatal(err)
	}
	a := agent.Agent{Tenant: "t1", ID: "rae", Model: &lookup{}, ModelID: "test-model", MaxTokens: 1024, MaxCost: 1_000_000,
		Tools: []tool.Tool{stripeGet{}}}
	out, _, err := agent.Run(ctx, a, recording, nil, r.Input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := recording.End(ctx); err != nil {
		t.Fatal(err)
	}
	r, err = run.Save(ctx, runs, r, now, func(n *run.Run) error {
		n.Status, n.Stop, n.Output = run.Completed, string(out.Stop), out.Text
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return r, s
}

// A run recorded under the eval policy loads with its status and its own
// events, and keeps its tool calls and results for the checks to read.
func TestARunLoadsWithWhatItDid(t *testing.T) {
	store, runs := memory.New(), memory.NewRuns()
	policy := eval.Policy(24 * time.Hour)
	if err := policy.Check(); err != nil {
		t.Fatal(err)
	}
	first, s := ran(t, store, runs, policy, session.Session{})
	second, _ := ran(t, store, runs, policy, s) // another run of the same session
	ctx := context.Background()
	for _, want := range []run.Run{first, second} {
		got, err := eval.Load(ctx, store, runs, "t1", want.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != run.Completed || got.Session.ID != s.ID {
			t.Fatalf("loaded %+v", got)
		}
		for _, e := range got.Events {
			if e.Run != want.ID {
				t.Fatalf("run %s loaded an event of run %s", want.ID, e.Run)
			}
		}
		calls, err := got.Calls()
		if err != nil || len(calls) != 1 || calls[0].Tool != "stripe_get" {
			t.Fatalf("calls %+v, %v", calls, err)
		}
		results, err := got.Results()
		if err != nil || results["c1"].Text != `{"amount":4200}` {
			t.Fatalf("results %+v, %v", results, err)
		}
		if report, _ := got.Report(); report != "ch_1 is 42 EUR." {
			t.Fatalf("report %q", report)
		}
	}
}

// Under the default policy the same run keeps no tool calls once it has
// ended: why eval runs need their own.
func TestTheDefaultPolicyLeavesNothingToGrade(t *testing.T) {
	store, runs := memory.New(), memory.NewRuns()
	r, _ := ran(t, store, runs, session.DefaultPolicy(24*time.Hour), session.Session{})
	got, err := eval.Load(context.Background(), store, runs, "t1", r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if calls, _ := got.Calls(); len(calls) != 0 {
		t.Fatalf("the default policy kept %d calls", len(calls))
	}
}

// Another tenant's run, or a missing one, doesn't load.
func TestLoadIsTheTenants(t *testing.T) {
	store, runs := memory.New(), memory.NewRuns()
	r, _ := ran(t, store, runs, eval.Policy(time.Hour), session.Session{})
	for _, c := range [][2]string{{"t2", r.ID}, {"t1", "missing"}} {
		if _, err := eval.Load(context.Background(), store, runs, c[0], c[1]); !errors.Is(err, run.ErrNotFound) {
			t.Fatalf("%v: err = %v, want ErrNotFound", c, err)
		}
	}
}
