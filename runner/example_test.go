package runner_test

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/runner"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
)

// replies is a model that answers each turn with the next reply.
type replies []model.Reply

func (r *replies) Turn(context.Context, model.Request, func(string)) (model.Reply, error) {
	next := (*r)[0]
	*r = (*r)[1:]
	return next, nil
}

// refund is a gateway tool whose every call returns result.
type refund struct{ result tool.Result }

func (refund) Spec() model.ToolSpec {
	return model.ToolSpec{Name: "stripe_refund", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (refund) Access() tool.Access { return tool.Write }
func (refund) Gateway() bool       { return true }
func (r refund) Call(context.Context, json.RawMessage) (tool.Result, error) {
	return r.result, nil
}

type secrets struct{}

func (secrets) Key(context.Context, string, string) (string, error) { return "kiff-agent-key", nil }

// newRunner is a runner on memory stores, for one agent, "rae", whose model
// says m and whose one tool returns result.
func newRunner(m *replies, result tool.Result) *runner.Runner {
	ctx := context.Background()
	agents := memory.NewAgents()
	_ = agents.Put(ctx, registry.Definition{
		Tenant: "t1", ID: "rae",
		Instructions: "You refund orders. Every write goes through KIFF.",
		Model:        "claude-opus-5-5", Price: model.Price{Input: 5_000_000, Output: 25_000_000},
		MaxTokens: 8192, Gateway: "https://mcp.kiff.dev/mcp", KeyRef: "rae",
		Keep:    session.DefaultPolicy(30 * 24 * time.Hour),
		MaxCost: 500_000, History: 20,
	})
	return &runner.Runner{
		Runs:     memory.NewRuns(),
		Sessions: memory.New(),
		Builder: &registry.Builder{
			Agents: agents, Secrets: secrets{}, Model: m,
			Gateways: []string{"https://mcp.kiff.dev/mcp"},
			Connect: func(context.Context, string, string) ([]tool.Tool, error) {
				return []tool.Tool{refund{result}}, nil
			},
		},
	}
}

func callRefund(id, input string) model.Reply {
	return model.Reply{Blocks: []model.Block{{Kind: model.ToolUse, CallID: id, Tool: "stripe_refund", Input: json.RawMessage(input)}},
		Stop: model.CallTools}
}

func answer(text string) model.Reply {
	return model.Reply{Blocks: []model.Block{{Kind: model.Text, Text: text}}, Stop: model.EndTurn}
}

// Start stores a PENDING run; Execute claims it, runs the loop and stores
// where it stopped.
func ExampleRunner() {
	ctx := context.Background()
	rn := newRunner(&replies{callRefund("c1", `{"order":42}`), answer("Refunded order 42.")}, tool.Result{Text: "refunded re_1"})

	r, _ := rn.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "Refund order 42."})
	fmt.Println(r.Status)

	r, _ = rn.Execute(ctx, "t1", r.ID, nil)
	fmt.Println(r.Status, r.Stop, r.Turns)
	fmt.Println(r.Output)
	// Output:
	// PENDING
	// COMPLETED answered 2
	// Refunded order 42.
}

// A call KIFF holds pauses the run: one requirement per held call.
func ExampleRunner_paused() {
	ctx := context.Background()
	rn := newRunner(&replies{callRefund("c1", `{"order":42}`), answer("The refund waits for your answer.")},
		tool.Result{Text: "Held for the owner.", IsError: true, State: tool.StateHeld})

	r, _ := rn.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "Refund order 42."})
	r, _ = rn.Execute(ctx, "t1", r.ID, nil)
	fmt.Println(r.Status, r.Output)
	for _, q := range r.Requirements {
		fmt.Println(q.CallID, q.Tool, string(q.Input))
	}
	// Output:
	// PAUSED The refund waits for your answer.
	// c1 stripe_refund {"order":42}
}
