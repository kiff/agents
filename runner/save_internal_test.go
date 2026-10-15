package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/store/memory"
)

// An execution whose claim was taken over (its run paused, then claimed by
// another invocation) saves nothing over the other invocation's run.
func TestASaveAfterAnotherClaimWritesNothing(t *testing.T) {
	ctx := context.Background()
	runs := memory.NewRuns()
	now := time.Now().UTC()
	rn := &Runner{Runs: runs, Now: func() time.Time { return now }}
	r := run.New("t1", "rae", "s1", run.ByOwner, "go", now)
	if err := runs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	r, err := run.Claim(ctx, runs, "t1", r.ID, "i1", now.Add(time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	first := &execution{rn: rn, tenant: "t1", id: r.ID, claim: "i1"}
	r, err = first.save(ctx, func(n *run.Run) error {
		n.Status = run.Paused
		n.Requirements = []run.Requirement{{CallID: "c1", Tool: "stripe_refund", Answered: now}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := run.Claim(ctx, runs, "t1", r.ID, "i2", now.Add(time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.save(ctx, func(n *run.Run) error { n.Output = "stale"; return nil }); !errors.Is(err, run.ErrLeaseLost) {
		t.Fatalf("a save after another claim: err = %v, want ErrLeaseLost", err)
	}
	if got, _ := runs.Get(ctx, "t1", r.ID); got.Output != "" || got.Version != second.Version || got.Claim != "i2" {
		t.Fatalf("stored %+v", got)
	}
}
