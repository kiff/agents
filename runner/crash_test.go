package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/runner"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/tool"
)

// kiffRefund is a refund through a fake KIFF that keeps each call's outcome
// by its operation ID, as KIFF's gateway does: a call repeated with the same
// ID gets its first outcome back and is never sent to the vendor twice.
// sent counts what reached the vendor.
type kiffRefund struct {
	mu       sync.Mutex
	outcomes map[string]tool.Result
	sent     int
	// crash, when set, is where the run's invocation stops on its first
	// call: "before" the call reaches KIFF, or "after" KIFF sent it, before
	// its result comes back. The call then never returns.
	crash  string
	stuck  chan struct{}
	inFlow chan struct{}
}

func newKIFFRefund(crash string) *kiffRefund {
	return &kiffRefund{outcomes: map[string]tool.Result{}, crash: crash, stuck: make(chan struct{}), inFlow: make(chan struct{})}
}

// send is KIFF's answer to the call op: its first outcome, or the vendor's.
func (k *kiffRefund) send(op string) tool.Result {
	k.mu.Lock()
	defer k.mu.Unlock()
	if res, ok := k.outcomes[op]; ok {
		return res
	}
	k.sent++
	res := tool.Result{Text: "refunded re_1"}
	k.outcomes[op] = res
	return res
}

func (k *kiffRefund) Spec() model.ToolSpec {
	return model.ToolSpec{Name: "stripe_refund", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (k *kiffRefund) Access() tool.Access { return tool.Write }
func (k *kiffRefund) Gateway() bool       { return true }
func (k *kiffRefund) Call(ctx context.Context, _ json.RawMessage) (tool.Result, error) {
	ref, _ := run.RefFrom(ctx)
	op := run.Operation(ref.Run, tool.CallFrom(ctx))
	switch k.crash {
	case "before":
		k.crash = ""
		close(k.inFlow)
		<-k.stuck
		return tool.Result{}, errors.New("the invocation is gone")
	case "after":
		k.crash = ""
		k.send(op)
		close(k.inFlow)
		<-k.stuck
		return tool.Result{}, errors.New("the invocation is gone")
	}
	return k.send(op), nil
}

// collect is the runner's Collect against the fake KIFF: each call repeated by
// the operation ID the run gives it.
func (k *kiffRefund) collect(_ context.Context, _ agent.Agent, held []run.Requirement) ([]agent.ToolResultBody, error) {
	var out []agent.ToolResultBody
	for _, q := range held {
		if q.Operation == "" {
			return nil, errors.New("no operation ID")
		}
		out = append(out, agent.ToolResultBody{CallID: q.CallID, Tool: q.Tool, Result: k.send(q.Operation)})
	}
	return out, nil
}

// A run whose invocation stops in a turn that acts is carried on by the sweep
// from that turn, and the refund is made exactly once: whether the
// invocation stopped before the call reached KIFF, or after KIFF sent it and
// before its result was stored.
func TestACrashedRunRefundsExactlyOnce(t *testing.T) {
	for _, crash := range []string{"before", "after"} {
		t.Run(crash, func(t *testing.T) {
			f := newFixture(t, use("c1", "stripe_refund", `{"payment":"pi_1"}`), say("Refunded pi_1."))
			k := newKIFFRefund(crash)
			t.Cleanup(func() { close(k.stuck) })
			f.r.Builder.Connect = func(context.Context, string, string) ([]tool.Tool, error) { return []tool.Tool{k}, nil }
			r := f.start(server.Start{Input: "refund pi_1"})
			go func() { _, _ = f.r.Execute(ctx, "t1", r.ID, nil) }()
			<-k.inFlow

			// The invocation is gone: its lease runs out, and the sweep carries
			// the run on in a fresh process.
			if stuck := f.get(r.ID); stuck.Status != run.Running || stuck.Acting == nil {
				t.Fatalf("while it crashed: %+v, want RUNNING with its acting turn", stuck)
			}
			f.clock.add(runner.MaxRun + run.LeaseGrace + time.Second)
			fresh := f.newRunner()
			fresh.Builder.Connect = f.r.Builder.Connect
			fresh.Collect = k.collect
			sw := run.Sweeper{Runs: f.runs, Ended: fresh.Ended, Hold: fresh.Held,
				Continue: func(ctx context.Context, tenant, id string) error {
					_, err := fresh.Execute(ctx, tenant, id, nil)
					return err
				}}
			if n, err := sw.Sweep(ctx, f.clock.now(), 10); err != nil || n != 1 {
				t.Fatalf("sweep: %d, %v", n, err)
			}
			got := f.get(r.ID)
			if got.Status != run.Completed || got.Output != "Refunded pi_1." || got.Resumes != 1 || got.Acting != nil {
				t.Fatalf("carried on to %+v", got)
			}
			if k.sent != 1 {
				t.Fatalf("the vendor got %d refunds, want exactly 1", k.sent)
			}
			if len(got.Done) != 1 || got.Done[0].CallID != "c1" {
				t.Fatalf("done %+v, want the one refund", got.Done)
			}
			// The model read the refund's outcome, said as one made once.
			reqs := f.model.requests()
			last := reqs[len(reqs)-1].Messages
			res := last[len(last)-1].Blocks[0]
			if res.Kind != model.ToolResult || res.CallID != "c1" || res.IsError {
				t.Fatalf("the model continued from %+v", last[len(last)-1])
			}
			// Nothing is left for a later sweep.
			if n, err := sw.Sweep(ctx, f.clock.now(), 10); err != nil || n != 0 {
				t.Fatalf("a second sweep: %d, %v", n, err)
			}
		})
	}
}
