package runner_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/runner"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/tool"
)

// grants is the account's answer, by subject, that a test changes as it
// goes; a subject missing is no answer at all.
type grants map[string]bool

func (g grants) access(_ context.Context, tenant, agent, subject string) (run.Access, error) {
	use, ok := g[subject]
	if !ok {
		return run.Access{}, errors.New("the account didn't answer")
	}
	return run.Access{Use: use}, nil
}

var (
	ana = run.Requester{Kind: run.ByMember, ID: "ana"}
	bo  = run.Requester{Kind: run.ByMember, ID: "bo"}
)

// #221: a member's run starts only when the account says they may use the
// agent; no answer is no. Runs no member asked for, and a door's sender's,
// start as before.
func TestAMembersRunNeedsTheirAccess(t *testing.T) {
	f := newFixture(t)
	g := grants{"ana": true, "bo": false}
	f.r.Access = g.access
	for _, c := range []struct {
		name string
		s    server.Start
		want error
	}{
		{"granted", server.Start{Input: "go", Requester: ana}, nil},
		{"not granted", server.Start{Input: "go", Requester: bo}, run.ErrNoAccess},
		{"no answer", server.Start{Input: "go", Requester: run.Requester{Kind: run.ByMember, ID: "cy"}}, run.ErrNoAccess},
		{"a door's sender", server.Start{Input: "go", Requester: run.Requester{Kind: run.ByDoor, ID: "d1"}}, nil},
		{"no one", server.Start{Input: "go"}, nil},
	} {
		c.s.Tenant, c.s.Agent, c.s.Trigger = "t1", "rae", run.ByOwner
		if _, err := f.r.Start(ctx, c.s); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
}

// #221: a member who loses the agent has their run stop: one stored ends
// CANCELLED, never started; one going stops at its next checkpoint, saying
// why; a paused one collects nothing, so no approved call is sent.
func TestAMembersRunStopsWhenTheyLoseAccess(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_refund", `{"order":42}`), say("Waiting for approval."), say("Done."))
	g := grants{"ana": true}
	f.r.Access = g.access

	stored := f.start(server.Start{Input: "go", Requester: ana})
	g["ana"] = false
	got, err := f.r.Execute(ctx, "t1", stored.ID, nil)
	if !errors.Is(err, run.ErrNoAccess) || got.Status != run.Cancelled || got.Error != runner.FailedNoAccess {
		t.Fatalf("a stored run of someone who lost the agent = %+v, %v", got, err)
	}

	// Going: the first turn's call is made, then the checkpoint finds the
	// grant gone and the run stops before its next model turn.
	g["ana"] = true
	going := f.start(server.Start{Input: "refund order 42", Requester: ana})
	f.refund.onCall = func() { g["ana"] = false }
	got, err = f.r.Execute(ctx, "t1", going.ID, nil)
	if err != nil || got.Status != run.Cancelled || got.Error != runner.FailedNoAccess {
		t.Fatalf("a going run of someone who lost the agent = %+v, %v", got, err)
	}
	if n := len(f.model.requests()); n != 1 {
		t.Errorf("model turns after the grant went: %d, want the one before", n)
	}
}

// #221: a paused run of someone who lost the agent collects nothing: an
// approved call isn't sent, and each held call says why.
func TestAPausedRunCollectsNothingWithoutAccess(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_refund", `{"order":42}`), say("Waiting for approval."), say("After."))
	g := grants{"ana": true}
	f.r.Access = g.access
	paused := f.pause(f.start(server.Start{Input: "refund order 42", Requester: ana}))
	f.answer(paused)
	g["ana"] = false
	collected := 0
	f.r.Collect = func(context.Context, agent.Agent, []run.Requirement) ([]agent.ToolResultBody, error) {
		collected++
		return []agent.ToolResultBody{{CallID: "c1", Tool: "stripe_refund", Result: tool.Result{Text: "refunded re_1"}}}, nil
	}
	var results []string
	got, err := f.r.Execute(ctx, "t1", paused.ID, func(e agent.Event) {
		if e.Kind == agent.CallResult && e.Result != nil {
			results = append(results, e.Result.Text)
		}
	})
	if err != nil || got.Status != run.Cancelled || got.Error != runner.FailedNoAccess || collected != 0 {
		t.Fatalf("a paused run of someone who lost the agent = %+v, %v (collected %d)", got, err, collected)
	}
	if len(results) != 1 || results[0] != runner.NotCollectedNoAccess {
		t.Errorf("held call results = %q", results)
	}
}
