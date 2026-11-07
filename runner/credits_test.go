package runner_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/server"
)

// No new run starts while the account's tafs wait for credits: KIFF's
// answer is asked for each one, with the run's agent, and a refusal stores
// nothing and carries KIFF's code. KIFF not answering fails the start. A run
// started before goes on, and a repeated delivery of a stored run doesn't
// ask at all.
func TestNoRunStartsWhileTheTafsWaitForCredits(t *testing.T) {
	f := newFixture(t, say("Done."), say("Again."))
	stored := f.start(server.Start{Input: "before the credits ran out", ID: "firing-1", Trigger: run.BySchedule})
	var asked []string
	answer := struct {
		ok  bool
		why string
		err error
	}{false, "limit_reached", nil}
	f.r.Allowance = func(_ context.Context, tenant, agent string) (bool, string, error) {
		asked = append(asked, tenant+"/"+agent)
		return answer.ok, answer.why, answer.err
	}
	for _, s := range []server.Start{
		{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "go"},
		{Tenant: "t1", Agent: "rae", Trigger: run.BySchedule, Input: "go", Source: "morning"},
		{Tenant: "t1", Agent: "rae", Trigger: run.ByTrial, Input: "try"},
	} {
		_, err := f.r.Start(ctx, s)
		var refusal *server.CreditsRefusal
		if !errors.As(err, &refusal) || refusal.Why != "limit_reached" {
			t.Fatalf("a %s run while the tafs wait: %v", s.Trigger, err)
		}
	}
	if len(asked) != 3 || asked[0] != "t1/rae" {
		t.Fatalf("asked %v", asked)
	}
	if runs, _ := f.runs.List(ctx, "t1", "rae", 10); len(runs) != 1 {
		t.Fatalf("a refused start stored a run: %d runs", len(runs))
	}
	// The run stored before goes on: Execute asks nothing.
	if got, err := f.r.Execute(ctx, "t1", stored.ID, nil); err != nil || got.Status != run.Completed || len(asked) != 3 {
		t.Fatalf("the run from before: %+v %v (asked %d)", got, err, len(asked))
	}
	// A repeated delivery of a stored run finds it, and asks nothing.
	if _, err := f.r.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.BySchedule, Input: "go", ID: "firing-1"}); !errors.Is(err, run.ErrExists) || len(asked) != 3 {
		t.Fatalf("a repeated firing: %v (asked %d)", err, len(asked))
	}
	// KIFF not answering: the start fails, as KIFF unanswered.
	answer.err = errors.New("KIFF is down")
	if _, err := f.r.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "go"}); err == nil || err.Error() != "KIFF didn't say whether a run may start" {
		t.Fatalf("KIFF down: %v", err)
	}
	// Allowed: it starts.
	answer.ok, answer.why, answer.err = true, "", nil
	if r, err := f.r.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "go"}); err != nil || r.Status != run.Pending {
		t.Fatalf("allowed: %+v %v", r, err)
	}
}
