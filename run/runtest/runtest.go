// Package runtest checks that a run.Runs keeps the promises the run package
// makes. Every store runs the same checks:
//
//	func TestRuns(t *testing.T) {
//		runtest.Run(t, func(t *testing.T, now func() time.Time) run.Runs { return newRuns(t, now) })
//	}
//
// newRuns gets each check's own clock, which is the store's clock for what has
// expired, and its own t, to clean up after the check.
package runtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
)

// Run runs every check, each against a fresh store from newRuns.
func Run(t *testing.T, newRuns func(t *testing.T, now func() time.Time) run.Runs) {
	checks := []struct {
		name string
		fn   func(*testing.T, run.Runs, *clock)
	}{
		{"CreateAndGet", createAndGet},
		{"TheTransitionTableHolds", theTransitionTableHolds},
		{"OneClaimWins", oneClaimWins},
		{"AStaleWriteIsRefused", aStaleWriteIsRefused},
		{"NoWriteAfterTheLease", noWriteAfterTheLease},
		{"ACancelKeepsTheClaimantsSave", aCancelKeepsTheClaimantsSave},
		{"ASaveAfterAnotherClaimIsRefused", aSaveAfterAnotherClaimIsRefused},
		{"ARunCancelledWhileHeldEndsOnceResolved", aRunCancelledWhileHeldEndsOnceResolved},
		{"ASaveGivesUpOnARunThatKeepsChanging", aSaveGivesUpOnARunThatKeepsChanging},
		{"APausedRunContinuesWhenResolved", aPausedRunContinuesWhenResolved},
		{"ABigPausedTurnIsKept", aBigPausedTurnIsKept},
		{"SweepMarksWhatIsDue", sweepMarksWhatIsDue},
		{"SweepLeavesWhatMovedOn", sweepLeavesWhatMovedOn},
		{"AContinuedRunThatCrashesIsSwept", aContinuedRunThatCrashesIsSwept},
		{"AFinishedRunKeepsNoTurn", aFinishedRunKeepsNoTurn},
		{"AnEditInPlaceIsStored", anEditInPlaceIsStored},
		{"ASweepNeverEndsAJustClaimedRun", aSweepNeverEndsAJustClaimedRun},
		{"SweptRunsExpireAndCancelsHold", sweptRunsExpireAndCancelsHold},
		{"ListNewestFirst", listNewestFirst},
		{"ListBySource", listBySource},
		{"ARunIsFoundByItsHold", aRunIsFoundByItsHold},
		{"AnAnswerIsRecordedOnce", anAnswerIsRecordedOnce},
		{"AnAnswerOutlivesTheRunsEnd", anAnswerOutlivesTheRunsEnd},
		{"WhatARunIsNeverChanges", whatARunIsNeverChanges},
		{"EndedRunsExpire", endedRunsExpire},
		{"TenantsAreApart", tenantsAreApart},
		{"DeleteAgentLeavesNothing", deleteAgentLeavesNothing},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			clock := start()
			c.fn(t, newRuns(t, clock.now), clock)
		})
	}
}

var ctx = context.Background()

// clock is a time that moves only when told. It starts at the real time, so
// a database that expires items by itself never sees a TTL already past.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }
func start() *clock                  { return &clock{t: time.Now().UTC().Truncate(time.Second)} }

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// created stores a new PENDING run of agent for tenant.
func created(t *testing.T, s run.Runs, c *clock, tenant, agent string) run.Run {
	t.Helper()
	r := run.New(tenant, agent, "s1", run.ByOwner, "refund order 12", c.now())
	check(t, s.Create(ctx, r))
	return r
}

func get(t *testing.T, s run.Runs, r run.Run) run.Run {
	t.Helper()
	got, err := s.Get(ctx, r.Tenant, r.ID)
	check(t, err)
	return got
}

// claimed is a run claimed by invocation "i1", whose deadline is 15 minutes on.
func claimed(t *testing.T, s run.Runs, c *clock) run.Run {
	t.Helper()
	r := created(t, s, c, "t1", "refunds")
	got, err := run.Claim(ctx, s, "t1", r.ID, "i1", c.now().Add(15*time.Minute), c.now())
	check(t, err)
	return got
}

func set(status run.Status) func(*run.Run) error {
	return func(n *run.Run) error { n.Status = status; return nil }
}

func createAndGet(t *testing.T, s run.Runs, c *clock) {
	r := created(t, s, c, "t1", "refunds")
	if got := get(t, s, r); !reflect.DeepEqual(got, r) {
		t.Fatalf("got %+v\nwant %+v", got, r)
	}
	if err := s.Create(ctx, r); !errors.Is(err, run.ErrExists) {
		t.Fatalf("creating it again: err = %v, want ErrExists", err)
	}
	if _, err := s.Get(ctx, "t1", "missing"); !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("a missing run: err = %v, want ErrNotFound", err)
	}
}

// Every move in a run's life goes through, and none other does: the final
// statuses go nowhere.
func theTransitionTableHolds(t *testing.T, s run.Runs, c *clock) {
	all := []run.Status{run.Pending, run.Running, run.Paused, run.Completed, run.Error, run.Cancelled}
	allowed := map[[2]run.Status]bool{
		{run.Pending, run.Running}: true, {run.Pending, run.Cancelled}: true, {run.Pending, run.Error}: true,
		{run.Running, run.Completed}: true, {run.Running, run.Error}: true, {run.Running, run.Cancelled}: true, {run.Running, run.Paused}: true,
		{run.Paused, run.Running}: true, {run.Paused, run.Cancelled}: true,
		{run.Pending, run.Pending}: true, {run.Running, run.Running}: true, {run.Paused, run.Paused}: true,
	}
	for _, from := range all {
		for _, to := range all {
			r := created(t, s, c, "t1", "refunds")
			if from != run.Pending { // put it in from first, straight on the store
				next := r
				next.Status, next.Version = from, r.Version+1
				check(t, s.Put(ctx, r, next))
				r = next
			}
			_, err := run.Change(ctx, s, r, c.now(), set(to))
			stored := get(t, s, r).Status
			switch {
			case allowed[[2]run.Status{from, to}] && (err != nil || stored != to):
				t.Errorf("%s → %s: err = %v, stored %s", from, to, err, stored)
			case !allowed[[2]run.Status{from, to}] && (!errors.Is(err, run.ErrTransition) || stored != from):
				t.Errorf("%s → %s went through: err = %v, stored %s", from, to, err, stored)
			}
		}
	}
}

// Two invocations claim one run at once: exactly one gets it.
func oneClaimWins(t *testing.T, s run.Runs, c *clock) {
	r := created(t, s, c, "t1", "refunds")
	const n = 8
	errs := make(chan error, n)
	for i := range n {
		go func() {
			_, err := run.Claim(ctx, s, "t1", r.ID, string(rune('a'+i)), c.now().Add(time.Minute), c.now())
			errs <- err
		}()
	}
	won := 0
	for range n {
		switch err := <-errs; {
		case err == nil:
			won++
		case errors.Is(err, run.ErrConflict), errors.Is(err, run.ErrTransition):
		default:
			t.Errorf("a losing claim: %v", err)
		}
	}
	got := get(t, s, r)
	if won != 1 || got.Status != run.Running || got.Claim == "" || got.Version != 2 {
		t.Fatalf("%d claims won; stored %+v", won, got)
	}
	if want := c.now().Add(time.Minute + run.LeaseGrace); !got.Lease.Equal(want) {
		t.Fatalf("lease %v, want %v: a minute past the invocation's deadline", got.Lease, want)
	}
}

// A write from a run as read before another change is refused, and stores
// nothing.
func aStaleWriteIsRefused(t *testing.T, s run.Runs, c *clock) {
	r := claimed(t, s, c)
	if _, err := run.Change(ctx, s, r, c.now(), func(n *run.Run) error { n.Cancel = true; return nil }); err != nil {
		t.Fatal(err)
	}
	_, err := run.Change(ctx, s, r, c.now(), set(run.Completed)) // r is the run as it was before the cancel
	if !errors.Is(err, run.ErrConflict) {
		t.Fatalf("stale write: err = %v, want ErrConflict", err)
	}
	if got := get(t, s, r); got.Status != run.Running || !got.Cancel || got.Version != r.Version+1 {
		t.Fatalf("stored %+v", got)
	}
}

// The invocation that claimed a run writes only while its lease lasts; once
// it ran out, or once the sweep marked the run, nothing it writes lands.
func noWriteAfterTheLease(t *testing.T, s run.Runs, c *clock) {
	r := claimed(t, s, c)
	r, err := run.Save(ctx, s, r, c.now(), func(n *run.Run) error { n.Cost = 100; return nil })
	check(t, err)
	if r.Claim != "i1" || r.Cost != 100 {
		t.Fatalf("saved %+v", r)
	}
	c.add(16*time.Minute + run.LeaseGrace)
	if _, err := run.Save(ctx, s, r, c.now(), set(run.Completed)); !errors.Is(err, run.ErrLeaseLost) {
		t.Fatalf("write after the lease: err = %v, want ErrLeaseLost", err)
	}
	if n, err := (run.Sweeper{Runs: s}).Sweep(ctx, c.now(), 10); err != nil || n != 1 {
		t.Fatalf("sweep marked %d, err = %v", n, err)
	}
	r.Lease = c.now().Add(time.Hour) // a clock gone wrong: the store still refuses
	if _, err := run.Save(ctx, s, r, c.now(), set(run.Completed)); !errors.Is(err, run.ErrLeaseLost) {
		t.Fatalf("write after the sweep: err = %v, want ErrLeaseLost", err)
	}
	if got := get(t, s, r); got.Status != run.Error || got.Claim != "" || !got.Lease.IsZero() || !strings.Contains(got.Error, "lease") {
		t.Fatalf("stored %+v", got)
	}
}

// A cancel set while the claimant works doesn't cost the claimant its save:
// Save reads the run again, keeps the cancel, writes the claimant's change,
// and hands back a run that shows the cancel.
func aCancelKeepsTheClaimantsSave(t *testing.T, s run.Runs, c *clock) {
	r := claimed(t, s, c)
	if _, err := run.Change(ctx, s, r, c.now(), func(n *run.Run) error { n.Cancel = true; return nil }); err != nil {
		t.Fatal(err)
	}
	applied := 0
	saved, err := run.Save(ctx, s, r, c.now(), func(n *run.Run) error { applied++; n.Cost += 100; return nil })
	check(t, err)
	if !saved.Cancel || saved.Cost != 100 || saved.Status != run.Running || saved.Claim != "i1" {
		t.Fatalf("saved %+v", saved)
	}
	if applied != 2 {
		t.Fatalf("change ran %d times, want once on the stale run and once on the stored one", applied)
	}
	if got := get(t, s, r); !reflect.DeepEqual(got, saved) {
		t.Fatalf("stored %+v\nreturned %+v", got, saved)
	}
	done, err := run.Save(ctx, s, saved, c.now(), set(run.Cancelled))
	check(t, err)
	if done.Status != run.Cancelled || done.Cost != 100 {
		t.Fatalf("ended %+v", done)
	}
}

// A run cancelled while KIFF holds one of its calls pauses, Cancel set, and
// waits for KIFF. Once KIFF has resolved it, it is claimed again, still asked
// to stop, so its claimant records the outcomes and ends it CANCELLED. A
// pending run asked to stop is never claimed.
func aRunCancelledWhileHeldEndsOnceResolved(t *testing.T, s run.Runs, c *clock) {
	r := claimed(t, s, c)
	if _, err := run.Change(ctx, s, r, c.now(), func(n *run.Run) error { n.Cancel = true; return nil }); err != nil {
		t.Fatal(err)
	}
	paused, err := run.Save(ctx, s, r, c.now(), func(n *run.Run) error { // from the stale copy
		n.Status = run.Paused
		n.Requirements = []run.Requirement{{CallID: "c1", Tool: "stripe_refund"}}
		return nil
	})
	check(t, err)
	if paused.Status != run.Paused || !paused.Cancel || paused.Claim != "" {
		t.Fatalf("paused %+v, want PAUSED, Cancel set", paused)
	}
	if _, err := run.Claim(ctx, s, "t1", r.ID, "i2", c.now().Add(15*time.Minute), c.now()); !errors.Is(err, run.ErrNotReady) {
		t.Fatalf("claiming it before KIFF answered: err = %v, want ErrNotReady", err)
	}
	_, err = run.Change(ctx, s, paused, c.now(), func(n *run.Run) error { n.Requirements[0].Answered = c.now(); return nil })
	check(t, err)
	again, err := run.Claim(ctx, s, "t1", r.ID, "i2", c.now().Add(15*time.Minute), c.now())
	check(t, err)
	if again.Status != run.Running || !again.Cancel || again.Claim != "i2" {
		t.Fatalf("claimed %+v, want RUNNING, still asked to stop", again)
	}
	done, err := run.Save(ctx, s, again, c.now(), set(run.Cancelled))
	check(t, err)
	if done.Status != run.Cancelled {
		t.Fatalf("ended %+v", done)
	}

	pending := created(t, s, c, "t1", "refunds")
	pending, err = run.Change(ctx, s, pending, c.now(), func(n *run.Run) error { n.Cancel = true; return nil })
	check(t, err)
	if _, err := run.Claim(ctx, s, "t1", pending.ID, "i3", c.now().Add(time.Minute), c.now()); !errors.Is(err, run.ErrCancelled) {
		t.Fatalf("claiming a pending run asked to stop: err = %v, want ErrCancelled", err)
	}
}

// A run that paused and was claimed by another invocation is no longer the
// first claimant's: its stale save writes nothing.
func aSaveAfterAnotherClaimIsRefused(t *testing.T, s run.Runs, c *clock) {
	first := claimed(t, s, c)
	paused, err := run.Save(ctx, s, first, c.now(), func(n *run.Run) error {
		n.Status = run.Paused
		n.Requirements = []run.Requirement{{CallID: "c1", Tool: "stripe_refund", Answered: c.now()}}
		return nil
	})
	check(t, err)
	second, err := run.Claim(ctx, s, "t1", paused.ID, "i2", c.now().Add(15*time.Minute), c.now())
	check(t, err)
	if _, err := run.Save(ctx, s, first, c.now(), func(n *run.Run) error { n.Cost = 999; return nil }); !errors.Is(err, run.ErrLeaseLost) {
		t.Fatalf("the first claimant's stale save: err = %v, want ErrLeaseLost", err)
	}
	if got := get(t, s, first); got.Claim != "i2" || got.Cost != 0 || got.Version != second.Version {
		t.Fatalf("stored %+v", got)
	}
}

// churning is a store whose run changes under every write: each Put first
// bumps the stored run, as a busy server might.
type churning struct {
	run.Runs
	puts int
}

func (c *churning) Put(cx context.Context, prev, next run.Run) error {
	c.puts++
	stored, err := c.Runs.Get(cx, prev.Tenant, prev.ID)
	if err != nil {
		return err
	}
	bumped := stored
	bumped.Version++
	bumped.Output = fmt.Sprintf("bump %d", c.puts)
	if err := c.Runs.Put(cx, stored, bumped); err != nil {
		return err
	}
	return c.Runs.Put(cx, prev, next)
}

// Save gives up on a run that changes under every write, rather than trying
// forever, and says it was a conflict, not a lost lease.
func aSaveGivesUpOnARunThatKeepsChanging(t *testing.T, s run.Runs, c *clock) {
	r := claimed(t, s, c)
	busy := &churning{Runs: s}
	_, err := run.Save(ctx, busy, r, c.now(), func(n *run.Run) error { n.Cost = 1; return nil })
	if !errors.Is(err, run.ErrConflict) || errors.Is(err, run.ErrLeaseLost) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if busy.puts != run.SaveTries {
		t.Fatalf("%d writes, want %d", busy.puts, run.SaveTries)
	}
}

// A paused run is claimed again, under the same ID, once KIFF has resolved
// every requirement: answered, or expired.
func aPausedRunContinuesWhenResolved(t *testing.T, s run.Runs, c *clock) {
	r := claimed(t, s, c)
	held := []run.Requirement{
		{CallID: "c1", Tool: "stripe_refund", Input: json.RawMessage(`{"charge":"ch_1"}`), Operation: "op_1", Expires: c.now().Add(10 * time.Minute)},
		{CallID: "c2", Tool: "stripe_refund", Input: json.RawMessage(`{"charge":"ch_2"}`), Operation: "op_2", Expires: c.now().Add(time.Hour)},
	}
	turn := &run.Turn{Reply: model.Message{Role: model.Assistant, Blocks: []model.Block{{Kind: model.ToolUse, CallID: "c1", Tool: "stripe_refund"}}}}
	r, err := run.Save(ctx, s, r, c.now(), func(n *run.Run) error {
		n.Status, n.Requirements, n.Paused = run.Paused, held, turn
		return nil
	})
	check(t, err)
	if got := get(t, s, r); got.Status != run.Paused || got.Claim != "" || len(got.Requirements) != 2 || !reflect.DeepEqual(got.Paused, turn) {
		t.Fatalf("paused: %+v", got)
	}
	if _, err := run.Claim(ctx, s, "t1", r.ID, "i2", c.now().Add(time.Minute), c.now()); !errors.Is(err, run.ErrNotReady) {
		t.Fatalf("claiming with both unresolved: err = %v, want ErrNotReady", err)
	}
	r, err = run.Change(ctx, s, r, c.now(), func(n *run.Run) error { n.Requirements[1].Answered = c.now(); return nil })
	check(t, err)
	if r.Requirements[1].Answered.IsZero() || get(t, s, r).Requirements[1].Answered.IsZero() {
		t.Fatal("the answer wasn't stored")
	}
	if _, err := run.Claim(ctx, s, "t1", r.ID, "i2", c.now().Add(time.Minute), c.now()); !errors.Is(err, run.ErrNotReady) {
		t.Fatalf("claiming with one unresolved: err = %v, want ErrNotReady", err)
	}
	c.add(10 * time.Minute) // c1's hold expires: refused, and resolved
	got, err := run.Claim(ctx, s, "t1", r.ID, "i2", c.now().Add(time.Minute), c.now())
	check(t, err)
	if got.ID != r.ID || got.Status != run.Running || got.Claim != "i2" || got.Paused == nil {
		t.Fatalf("continued: %+v", got)
	}
}

// The turn a run paused in is kept whole, however big what the tools
// returned: the run continues from it.
func aBigPausedTurnIsKept(t *testing.T, s run.Runs, c *clock) {
	r := claimed(t, s, c)
	big := strings.Repeat("x", 250<<10) // tool results are capped at 256 KB each
	turn := &run.Turn{Reply: model.Message{Role: model.Assistant}}
	for range 4 {
		turn.Results = append(turn.Results, model.Block{Kind: model.ToolResult, CallID: "c", Result: big})
	}
	r, err := run.Save(ctx, s, r, c.now(), func(n *run.Run) error { n.Status, n.Paused = run.Paused, turn; return nil })
	check(t, err)
	if got := get(t, s, r); !reflect.DeepEqual(got.Paused, turn) {
		t.Fatalf("the paused turn came back with %d results", len(got.Paused.Results))
	}
	small := &run.Turn{Reply: model.Message{Role: model.Assistant}, Results: []model.Block{{Kind: model.ToolResult, Result: "ok"}}}
	r, err = run.Change(ctx, s, r, c.now(), func(n *run.Run) error { n.Paused = small; return nil })
	check(t, err)
	if got := get(t, s, r); !reflect.DeepEqual(got.Paused, small) {
		t.Fatalf("a smaller turn came back as %+v", got.Paused)
	}
}

// The sweep marks ERROR a RUNNING run whose lease ran out and a PENDING run
// nobody claimed in time, oldest first; nothing else.
func sweepMarksWhatIsDue(t *testing.T, s run.Runs, c *clock) {
	pending := created(t, s, c, "t1", "refunds")
	running := claimed(t, s, c) // lease: 16 minutes on
	paused := claimed(t, s, c)
	paused, err := run.Save(ctx, s, paused, c.now(), set(run.Paused))
	check(t, err)
	if due, err := s.Due(ctx, c.now(), 10); err != nil || len(due) != 0 {
		t.Fatalf("due now: %d, err = %v", len(due), err)
	}
	c.add(run.PendingTimeout + time.Second)
	due, err := s.Due(ctx, c.now(), 10)
	check(t, err)
	if len(due) != 1 || due[0].ID != pending.ID {
		t.Fatalf("due once the pending wait is over: %v", ids(due))
	}
	c.add(16 * time.Minute)
	var ended []string
	sw := run.Sweeper{Runs: s, Ended: func(_ context.Context, r run.Run) error { ended = append(ended, r.ID); return nil }}
	n, err := sw.Sweep(ctx, c.now(), 10)
	check(t, err)
	if n != 2 || !slices.Equal(ended, []string{running.ID}) {
		t.Fatalf("swept %d, ended %v, want both marked and only the running one's recording ended: a pending run has none", n, ended)
	}
	for _, r := range []run.Run{pending, running} {
		if got := get(t, s, r); got.Status != run.Error || got.Error == "" {
			t.Fatalf("after the sweep: %+v", got)
		}
	}
	if got := get(t, s, paused); got.Status != run.Paused {
		t.Fatalf("a paused run was swept: %+v", got)
	}
	if due, _ := s.Due(ctx, c.now(), 10); len(due) != 0 {
		t.Fatalf("still due after the sweep: %v", ids(due))
	}
}

// A run that moved on between the sweep's read and its write is left as it
// is; a run whose recording couldn't be ended stays due, for the next sweep.
func sweepLeavesWhatMovedOn(t *testing.T, s run.Runs, c *clock) {
	r := claimed(t, s, c)
	c.add(16*time.Minute + run.LeaseGrace)
	broken := errors.New("store down")
	if n, err := (run.Sweeper{Runs: s, Ended: func(context.Context, run.Run) error { return broken }}).Sweep(ctx, c.now(), 10); n != 0 || !errors.Is(err, broken) {
		t.Fatalf("swept %d, err = %v", n, err)
	}
	if got := get(t, s, r); got.Status != run.Running {
		t.Fatalf("marked although its recording wasn't ended: %+v", got)
	}
	n, err := (run.Sweeper{Runs: s, Ended: func(context.Context, run.Run) error {
		_, err := run.Change(ctx, s, get(t, s, r), c.now(), func(n *run.Run) error { n.Cost = 7; return nil }) // a write just now
		return err
	}}).Sweep(ctx, c.now(), 10)
	if err != nil || n != 0 {
		t.Fatalf("swept %d, err = %v", n, err)
	}
	if got := get(t, s, r); got.Status != run.Running || got.Cost != 7 {
		t.Fatalf("the run was swept over a newer write: %+v", got)
	}
}

// A run continued from PAUSED still holds its paused turn until its first
// Save; if its invocation crashes before that, the sweep still marks it, and
// the turn goes with it.
func aContinuedRunThatCrashesIsSwept(t *testing.T, s run.Runs, c *clock) {
	r := claimed(t, s, c)
	turn := &run.Turn{Results: []model.Block{{Kind: model.ToolResult, CallID: "c1", Result: strings.Repeat("x", 400<<10)}}}
	r, err := run.Save(ctx, s, r, c.now(), func(n *run.Run) error { n.Status, n.Paused = run.Paused, turn; return nil })
	check(t, err)
	r, err = run.Claim(ctx, s, "t1", r.ID, "i2", c.now().Add(15*time.Minute), c.now())
	check(t, err)
	if r.Paused == nil {
		t.Fatal("the continued run lost its turn")
	}
	c.add(16*time.Minute + run.LeaseGrace) // i2 crashed
	if n, err := (run.Sweeper{Runs: s}).Sweep(ctx, c.now(), 10); err != nil || n != 1 {
		t.Fatalf("swept %d, err = %v", n, err)
	}
	if got := get(t, s, r); got.Status != run.Error || got.Paused != nil {
		t.Fatalf("after the sweep: %s, paused turn kept: %v", got.Status, got.Paused != nil)
	}
}

// A run that ends, by the sweep or otherwise, keeps nothing of what tools
// returned: its paused turn goes.
func aFinishedRunKeepsNoTurn(t *testing.T, s run.Runs, c *clock) {
	r := claimed(t, s, c)
	turn := &run.Turn{Results: []model.Block{{Kind: model.ToolResult, Result: "customer data"}}}
	r, err := run.Save(ctx, s, r, c.now(), func(n *run.Run) error { n.Status, n.Paused = run.Paused, turn; return nil })
	check(t, err)
	r, err = run.Change(ctx, s, r, c.now(), set(run.Cancelled))
	check(t, err)
	if r.Paused != nil || get(t, s, r).Paused != nil {
		t.Fatal("a cancelled run kept its paused turn")
	}
}

// A change may edit the paused turn or a requirement in place: what it edits
// is stored, and the run it started from is left as it was.
func anEditInPlaceIsStored(t *testing.T, s run.Runs, c *clock) {
	r := claimed(t, s, c)
	turn := &run.Turn{Reply: model.Message{Role: model.Assistant, Blocks: []model.Block{{Kind: model.ToolUse, CallID: "c1", Input: json.RawMessage(`{"a":1}`)}}},
		Results: []model.Block{{Kind: model.ToolResult, CallID: "c0", Result: "before"}}}
	held := []run.Requirement{{CallID: "c1", Tool: "stripe_refund", Input: json.RawMessage(`{"a":1}`), Operation: "op_1", Expires: c.now().Add(time.Hour)}}
	r, err := run.Save(ctx, s, r, c.now(), func(n *run.Run) error { n.Status, n.Paused, n.Requirements = run.Paused, turn, held; return nil })
	check(t, err)
	r = get(t, s, r)
	next, err := run.Change(ctx, s, r, c.now(), func(n *run.Run) error {
		n.Paused.Results[0].Result = "after"
		n.Paused.Reply.Blocks[0].Input[5] = '2'
		n.Requirements[0].Input[5] = '2'
		return nil
	})
	check(t, err)
	if r.Paused.Results[0].Result != "before" || string(r.Paused.Reply.Blocks[0].Input) != `{"a":1}` || string(r.Requirements[0].Input) != `{"a":1}` {
		t.Fatalf("the change reached the run it started from: %+v", r)
	}
	got := get(t, s, next)
	if got.Version != r.Version+1 || got.Paused.Results[0].Result != "after" || string(got.Paused.Reply.Blocks[0].Input) != `{"a":2}` || string(got.Requirements[0].Input) != `{"a":2}` {
		t.Fatalf("stored: version %d, result %q, call input %s, requirement input %s",
			got.Version, got.Paused.Results[0].Result, got.Paused.Reply.Blocks[0].Input, got.Requirements[0].Input)
	}
}

// stale is a store whose Due answers with what it read before a claim landed.
type stale struct {
	run.Runs
	due []run.Run
}

func (s stale) Due(context.Context, time.Time, int) ([]run.Run, error) { return s.due, nil }

// A run claimed after the sweep read it as due is neither marked nor has its
// recording ended: the sweep reads it again, and it's no longer due.
func aSweepNeverEndsAJustClaimedRun(t *testing.T, s run.Runs, c *clock) {
	for _, from := range []run.Status{run.Pending, run.Running} {
		var r run.Run
		if from == run.Pending {
			r = created(t, s, c, "t1", "refunds")
			c.add(run.PendingTimeout + time.Second)
		} else {
			r = claimed(t, s, c)
			c.add(16*time.Minute + run.LeaseGrace)
		}
		all, err := s.Due(ctx, c.now(), 10)
		check(t, err)
		due := slices.DeleteFunc(all, func(d run.Run) bool { return d.ID != r.ID }) // only this pass's run
		if len(due) != 1 {
			t.Fatalf("%s: the run isn't due: %v", from, ids(all))
		}
		if from == run.Running { // a run whose lease ran out isn't claimed again, but it may be cancelled
			_, err = run.Change(ctx, s, get(t, s, r), c.now(), func(n *run.Run) error { n.Status = run.Cancelled; return nil })
		} else {
			_, err = run.Claim(ctx, s, "t1", r.ID, "late", c.now().Add(15*time.Minute), c.now())
		}
		check(t, err)
		ended := 0
		sw := run.Sweeper{Runs: stale{s, due}, Ended: func(context.Context, run.Run) error { ended++; return nil }}
		if n, err := sw.Sweep(ctx, c.now(), 10); err != nil || n != 0 || ended != 0 {
			t.Fatalf("%s: swept %d, ended %d, err = %v: the run had moved on", from, n, ended, err)
		}
		if got := get(t, s, r); got.Status == run.Error {
			t.Fatalf("%s: marked ERROR after it moved on", from)
		}
	}
}

// The sweep gives what it ends an expiry from the caller's keep rules; a run
// that was asked to stop ends CANCELLED, as asked.
func sweptRunsExpireAndCancelsHold(t *testing.T, s run.Runs, c *clock) {
	plain := created(t, s, c, "t1", "refunds")
	stopped := created(t, s, c, "t1", "refunds")
	stopped, err := run.Change(ctx, s, stopped, c.now(), func(n *run.Run) error { n.Cancel = true; return nil })
	check(t, err)
	c.add(run.PendingTimeout + time.Second)
	sw := run.Sweeper{Runs: s, Expires: func(r run.Run) time.Time { return c.now().Add(24 * time.Hour) }}
	if n, err := sw.Sweep(ctx, c.now(), 10); err != nil || n != 2 {
		t.Fatalf("swept %d, err = %v", n, err)
	}
	if got := get(t, s, stopped); got.Status != run.Cancelled {
		t.Fatalf("a run asked to stop ended %s, want CANCELLED", got.Status)
	}
	if got := get(t, s, plain); got.Status != run.Error || !got.Expires.Equal(c.now().Add(24*time.Hour)) {
		t.Fatalf("swept run: %s, expires %v", got.Status, got.Expires)
	}
	c.add(24*time.Hour + time.Second)
	for _, r := range []run.Run{plain, stopped} {
		if _, err := s.Get(ctx, "t1", r.ID); !errors.Is(err, run.ErrNotFound) {
			t.Fatalf("a swept run past its expiry: err = %v, want ErrNotFound", err)
		}
	}
}

func ids(runs []run.Run) []string {
	var out []string
	for _, r := range runs {
		out = append(out, r.ID)
	}
	return out
}

func listNewestFirst(t *testing.T, s run.Runs, c *clock) {
	var made []string
	for range 5 {
		made = append(made, created(t, s, c, "t1", "refunds").ID)
		c.add(time.Second)
	}
	created(t, s, c, "t1", "chargebacks")
	slices.Reverse(made)
	l, err := s.List(ctx, "t1", "refunds", 0)
	check(t, err)
	if !slices.Equal(ids(l), made) {
		t.Fatalf("list = %v, want %v", ids(l), made)
	}
	l, err = s.List(ctx, "t1", "refunds", 2)
	check(t, err)
	if !slices.Equal(ids(l), made[:2]) {
		t.Fatalf("limit 2: %v", ids(l))
	}
}

// sourced stores a new PENDING run of agent that source started.
func sourced(t *testing.T, s run.Runs, c *clock, tenant, agent string, trigger run.Trigger, source string) run.Run {
	t.Helper()
	r := run.New(tenant, agent, "s1", trigger, "daily report", c.now())
	r.Source = source
	check(t, s.Create(ctx, r))
	return r
}

// A source lists the runs it started, newest first, and only those: not
// another source's, not the same ID under another trigger, not another
// tenant's. Its list follows the runs as they end, expire and are removed.
func listBySource(t *testing.T, s run.Runs, c *clock) {
	var made []string
	for range 3 {
		made = append(made, sourced(t, s, c, "t1", "refunds", run.BySchedule, "sch1").ID)
		c.add(time.Second)
	}
	other := sourced(t, s, c, "t1", "chargebacks", run.BySchedule, "sch1") // another agent, the same schedule: listed
	sourced(t, s, c, "t1", "refunds", run.BySchedule, "sch2")
	sourced(t, s, c, "t1", "refunds", run.ByEvent, "sch1")
	sourced(t, s, c, "t2", "refunds", run.BySchedule, "sch1")
	created(t, s, c, "t1", "refunds")
	l, err := s.ListSource(ctx, "t1", run.BySchedule, "sch1", 0)
	check(t, err)
	if want := []string{other.ID, made[2], made[1], made[0]}; !slices.Equal(ids(l), want) {
		t.Fatalf("listed %v, want %v", ids(l), want)
	}
	for _, r := range l {
		if r.Tenant != "t1" || r.Trigger != run.BySchedule || r.Source != "sch1" {
			t.Fatalf("listed a run another source started: %+v", r)
		}
	}
	l, err = s.ListSource(ctx, "t1", run.BySchedule, "sch1", 2)
	check(t, err)
	if len(l) != 2 {
		t.Fatalf("limit 2: %v", ids(l))
	}
	if l, _ := s.ListSource(ctx, "t1", run.ByOwner, "", 0); len(l) != 0 {
		t.Fatalf("an empty source lists %v", ids(l))
	}

	// One ends and expires, one is deleted, and the agent goes: the list
	// keeps up with each.
	r, err := run.Claim(ctx, s, "t1", made[0], "i1", c.now().Add(time.Minute), c.now())
	check(t, err)
	_, err = run.Save(ctx, s, r, c.now(), func(n *run.Run) error {
		n.Status, n.Expires = run.Completed, c.now().Add(time.Hour)
		return nil
	})
	check(t, err)
	if l, _ := s.ListSource(ctx, "t1", run.BySchedule, "sch1", 0); len(l) != 4 {
		t.Fatalf("an ended run left its source's list: %v", ids(l))
	}
	c.add(time.Hour + time.Second)
	check(t, s.Delete(ctx, "t1", made[1]))
	if l, _ := s.ListSource(ctx, "t1", run.BySchedule, "sch1", 0); !slices.Equal(ids(l), []string{other.ID, made[2]}) {
		t.Fatalf("after one expired and one was deleted: %v", ids(l))
	}
	check(t, s.DeleteAgent(ctx, "t1", "refunds"))
	if l, _ := s.ListSource(ctx, "t1", run.BySchedule, "sch1", 0); !slices.Equal(ids(l), []string{other.ID}) {
		t.Fatalf("after the agent was removed: %v", ids(l))
	}
	if l, _ := s.ListSource(ctx, "t2", run.BySchedule, "sch1", 0); len(l) != 1 {
		t.Fatalf("t2's run went with t1's agent: %v", ids(l))
	}
}

// What a run is (its tenant, ID, agent, session, trigger, source, creation
// time) never changes: Change refuses it, and so does the store, so a run
// never leaves a list it's in or lands in one it isn't.
func whatARunIsNeverChanges(t *testing.T, s run.Runs, c *clock) {
	r := sourced(t, s, c, "t1", "refunds", run.BySchedule, "sch1")
	edits := map[string]func(*run.Run){
		"agent":   func(n *run.Run) { n.Agent = "chargebacks" },
		"session": func(n *run.Run) { n.Session = "s2" },
		"trigger": func(n *run.Run) { n.Trigger = run.ByEvent },
		"source":  func(n *run.Run) { n.Source = "sch2" },
		"created": func(n *run.Run) { n.Created = n.Created.Add(time.Second) },
	}
	for name, edit := range edits {
		if _, err := run.Change(ctx, s, r, c.now(), func(n *run.Run) error { edit(n); return nil }); !errors.Is(err, run.ErrFixed) {
			t.Fatalf("Change of the %s: err = %v, want ErrFixed", name, err)
		}
		next := r
		next.Version++
		edit(&next)
		if err := s.Put(ctx, r, next); !errors.Is(err, run.ErrFixed) {
			t.Fatalf("Put of the %s: err = %v, want ErrFixed", name, err)
		}
	}
	// A prev that wasn't read from the store, changed alike in prev and
	// next, is refused against the stored run.
	for name, edit := range edits {
		prev := r
		edit(&prev)
		next := prev
		next.Version++
		if err := s.Put(ctx, prev, next); !errors.Is(err, run.ErrFixed) {
			t.Fatalf("Put of a forged %s: err = %v, want ErrFixed", name, err)
		}
	}
	if got := get(t, s, r); !reflect.DeepEqual(got, r) {
		t.Fatalf("the run changed: %+v", got)
	}
	if l, _ := s.ListSource(ctx, "t1", run.BySchedule, "sch1", 0); !slices.Equal(ids(l), []string{r.ID}) {
		t.Fatalf("its source lists %v", ids(l))
	}
	if l, _ := s.ListSource(ctx, "t1", run.BySchedule, "sch2", 0); len(l) != 0 {
		t.Fatalf("another source lists %v", ids(l))
	}
}

// A run that has ended goes once its Expires has passed; one that hasn't
// ended never expires.
func endedRunsExpire(t *testing.T, s run.Runs, c *clock) {
	done := claimed(t, s, c)
	done, err := run.Save(ctx, s, done, c.now(), func(n *run.Run) error {
		n.Status, n.Expires = run.Completed, c.now().Add(24*time.Hour)
		return nil
	})
	check(t, err)
	paused := claimed(t, s, c)
	_, err = run.Save(ctx, s, paused, c.now(), func(n *run.Run) error {
		n.Status, n.Expires = run.Paused, c.now().Add(time.Hour) // ignored: it hasn't ended
		return nil
	})
	check(t, err)
	c.add(24*time.Hour + time.Second)
	if _, err := s.Get(ctx, "t1", done.ID); !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("an expired run: err = %v, want ErrNotFound", err)
	}
	if l, _ := s.List(ctx, "t1", "refunds", 0); !slices.Equal(ids(l), []string{paused.ID}) {
		t.Fatalf("list = %v, want only the paused run", ids(l))
	}
	if got := get(t, s, paused); got.Status != run.Paused {
		t.Fatalf("paused: %+v", got)
	}
}

func tenantsAreApart(t *testing.T, s run.Runs, c *clock) {
	r := created(t, s, c, "t1", "refunds")
	if _, err := s.Get(ctx, "t2", r.ID); !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("t2 read t1's run: err = %v", err)
	}
	if _, err := run.Claim(ctx, s, "t2", r.ID, "i", c.now().Add(time.Minute), c.now()); !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("t2 claimed t1's run: err = %v", err)
	}
	other := r
	other.Tenant = "t2"
	next := other
	next.Version++
	if err := s.Put(ctx, other, next); !errors.Is(err, run.ErrConflict) && !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("t2 wrote t1's run: err = %v", err)
	}
	if l, _ := s.List(ctx, "t2", "refunds", 0); len(l) != 0 {
		t.Fatalf("t2 lists t1's runs")
	}
	if err := s.Delete(ctx, "t2", r.ID); !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("t2 deleted t1's run: err = %v", err)
	}
	check(t, s.DeleteAgent(ctx, "t2", "refunds"))
	if got := get(t, s, r); !reflect.DeepEqual(got, r) {
		t.Fatalf("t1's run changed: %+v", got)
	}
}

func deleteAgentLeavesNothing(t *testing.T, s run.Runs, c *clock) {
	gone := claimed(t, s, c)
	pending := created(t, s, c, "t1", "refunds")
	kept := created(t, s, c, "t1", "chargebacks")
	big := &run.Turn{Results: []model.Block{{Kind: model.ToolResult, Result: strings.Repeat("x", 500<<10)}}}
	_, err := run.Save(ctx, s, gone, c.now(), func(n *run.Run) error { n.Status, n.Paused = run.Paused, big; return nil })
	check(t, err)
	check(t, s.DeleteAgent(ctx, "t1", "refunds"))
	for _, r := range []run.Run{gone, pending} {
		if _, err := s.Get(ctx, "t1", r.ID); !errors.Is(err, run.ErrNotFound) {
			t.Fatalf("a removed agent's run: err = %v", err)
		}
	}
	if l, _ := s.List(ctx, "t1", "refunds", 0); len(l) != 0 {
		t.Fatalf("a removed agent still lists %d runs", len(l))
	}
	c.add(time.Hour)
	if due, _ := s.Due(ctx, c.now(), 10); len(due) != 1 || due[0].ID != kept.ID {
		t.Fatalf("due after removal: %v, want only the other agent's run", ids(due))
	}
	check(t, s.Delete(ctx, "t1", kept.ID))
	if _, err := s.Get(ctx, "t1", kept.ID); !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("a deleted run: err = %v", err)
	}
	if due, _ := s.Due(ctx, c.now(), 10); len(due) != 0 {
		t.Fatalf("a deleted run is still due")
	}
}

// heldOn is a run claimed and paused on one call, which KIFF holds as
// exception.
func heldOn(t *testing.T, s run.Runs, c *clock, exception string) run.Run {
	t.Helper()
	r, err := run.Save(ctx, s, claimed(t, s, c), c.now(), func(n *run.Run) error {
		n.Status = run.Paused
		n.Requirements = []run.Requirement{{CallID: "c1", Tool: "stripe_refund", Operation: "op-1", Exception: exception,
			Expires: c.now().Add(10 * time.Minute)}}
		return nil
	})
	check(t, err)
	return r
}

// A run is found by the hold KIFF names, in its tenant only. A hold no run
// has paused on is ErrNotFound.
func aRunIsFoundByItsHold(t *testing.T, s run.Runs, c *clock) {
	r := heldOn(t, s, c, "exc_1")
	got, err := s.ByHold(ctx, "t1", "exc_1")
	check(t, err)
	if got.ID != r.ID || got.Status != run.Paused {
		t.Fatalf("found %+v", got)
	}
	for _, k := range [][2]string{{"t2", "exc_1"}, {"t1", "exc_2"}} {
		if _, err := s.ByHold(ctx, k[0], k[1]); !errors.Is(err, run.ErrNotFound) {
			t.Fatalf("%v: err = %v, want ErrNotFound", k, err)
		}
	}
	check(t, s.Delete(ctx, "t1", r.ID))
	if _, err := s.ByHold(ctx, "t1", "exc_1"); !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("a deleted run's hold: err = %v", err)
	}
}

// KIFF's answer is recorded on the requirement once: a repeat changes
// nothing. An answer for a hold no run has paused on yet is ErrNotFound, so
// the alert comes again.
func anAnswerIsRecordedOnce(t *testing.T, s run.Runs, c *clock) {
	if _, err := run.Answer(ctx, s, "t1", "exc_1", "approved", c.now()); !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("an answer before the pause: err = %v, want ErrNotFound", err)
	}
	r := heldOn(t, s, c, "exc_1")
	answered, err := run.Answer(ctx, s, "t1", "exc_1", "approved", c.now())
	check(t, err)
	q := answered.Requirements[0]
	if !q.Answered.Equal(c.now()) || q.Answer != "approved" || answered.Version != r.Version+1 || !q.Resolved(c.now()) {
		t.Fatalf("answered %+v", answered)
	}
	c.add(time.Minute)
	again, err := run.Answer(ctx, s, "t1", "exc_1", "refused", c.now())
	check(t, err)
	if !reflect.DeepEqual(again, answered) || !reflect.DeepEqual(get(t, s, r), answered) {
		t.Fatalf("a repeat changed the run: %+v", again)
	}
	if _, err := run.Claim(ctx, s, "t1", r.ID, "i2", c.now().Add(time.Minute), c.now()); err != nil {
		t.Fatalf("claiming it once answered: %v", err)
	}
}

// A run that ended before KIFF's answer came is still found by its hold, and
// the answer changes nothing; the hold goes when the run does.
func anAnswerOutlivesTheRunsEnd(t *testing.T, s run.Runs, c *clock) {
	r := heldOn(t, s, c, "exc_1")
	r, err := run.Claim(ctx, s, "t1", r.ID, "i2", c.now().Add(time.Hour), c.now().Add(11*time.Minute))
	check(t, err)
	c.add(11 * time.Minute) // the hold expired: resolved
	ended, err := run.Save(ctx, s, r, c.now(), func(n *run.Run) error {
		n.Status, n.Requirements, n.Expires = run.Completed, nil, c.now().Add(time.Hour)
		return nil
	})
	check(t, err)
	got, err := run.Answer(ctx, s, "t1", "exc_1", "expired", c.now())
	check(t, err)
	if !reflect.DeepEqual(got, ended) {
		t.Fatalf("an answer for an ended run changed it: %+v", got)
	}
	c.add(time.Hour + time.Second)
	if _, err := s.ByHold(ctx, "t1", "exc_1"); !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("an expired run's hold: err = %v", err)
	}

	// The sweep ends a run with its requirements still on it: an answer
	// then changes nothing either, and is no error (KIFF would deliver it
	// again for nothing).
	held := heldOn(t, s, c, "exc_2")
	held, err = run.Claim(ctx, s, "t1", held.ID, "i3", c.now(), c.now().Add(11*time.Minute))
	check(t, err)
	c.add(11*time.Minute + run.LeaseGrace + time.Second)
	if _, err := (run.Sweeper{Runs: s}).Sweep(ctx, c.now(), 10); err != nil {
		t.Fatal(err)
	}
	swept := get(t, s, held)
	if swept.Status != run.Error || len(swept.Requirements) != 1 {
		t.Fatalf("swept %+v", swept)
	}
	got, err = run.Answer(ctx, s, "t1", "exc_2", "approved", c.now())
	if err != nil || !reflect.DeepEqual(got, swept) {
		t.Fatalf("an answer for a swept run: %+v, %v", got, err)
	}
}
