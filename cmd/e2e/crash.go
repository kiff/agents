package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	runs "github.com/kiff/agents/run"
	"github.com/kiff/agents/runner"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/kiff"
)

// crashMidRefund is the reliability round's proof (kiff/tafs docs/reliability.md,
// "How it's proven"): a door message asks the taf for a refund inside its Card,
// the invocation stops once the vendor has the refund and before the run
// records its result, and the door sends the message again. A fresh
// invocation and the sweep carry the run on from its last safe step, and the
// refund's outcome is reconciled through KIFF by its operation ID, never sent
// again. The vendor then has exactly one refund for the charge, the run
// completes, the door's receiver gets exactly one end note (the one reply
// tafs cloud sends), and the second send started no second run.
func (t *test) crashMidRefund(ctx context.Context, tafID string) error {
	charge := "ch_e2e_crash_" + t.run
	key := "e2e-crash-" + t.run + "@mail"

	// The door's receiver, as tafs cloud's /v1/runs/ended: every note it gets.
	var mu sync.Mutex
	var notes []runs.Note
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n runs.Note
		_ = json.NewDecoder(r.Body).Decode(&n)
		mu.Lock()
		notes = append(notes, n)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()
	store := runs.Notifying{Runs: memory.NewRuns(), Client: receiver.Client()}

	// The invocation that stops: its refund goes through KIFF to the vendor,
	// and its result never comes back, so nothing after it is recorded.
	stuck, sent := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer close(stuck)
	crashing := *t.rn
	crashing.Runs = store
	builder := *t.rn.Builder
	connect := builder.Connect
	builder.Connect = func(ctx context.Context, gateway, k string) ([]tool.Tool, error) {
		tools, err := connect(ctx, gateway, k)
		for i, x := range tools {
			tools[i] = stopsAfterSend{Tool: x, charge: charge, sent: func() { once.Do(func() { close(sent) }) }, stuck: stuck}
		}
		return tools, err
	}
	crashing.Builder = &builder

	start := server.Start{Tenant: t.tenant, Agent: tafID, Trigger: runs.ByOwner, Source: "email",
		Requester: runs.Requester{Kind: runs.ByDoor, ID: "e2e-thread-" + t.run},
		Input:     "[door email · from Ana <ana@example.com>] Please refund charge " + charge + " for 300 cents.",
		Notify:    runs.Notify{URL: receiver.URL, Ref: "e2e-crash"},
		ID:        server.RequestRunID(t.tenant, tafID, key)}
	r, err := crashing.Start(ctx, start)
	if err != nil {
		return fmt.Errorf("the door's run: %w", err)
	}
	go func() { _, _ = crashing.Execute(ctx, t.tenant, r.ID, nil) }()
	select {
	case <-sent:
	case <-time.After(4 * time.Minute):
		return errors.New("the door's run never sent its refund to the vendor")
	}
	stopped, err := store.Get(ctx, t.tenant, r.ID)
	if err != nil {
		return err
	}
	if stopped.Status != runs.Running || stopped.Acting == nil {
		return fmt.Errorf("after the stop the run is %s (acting turn kept: %v), want RUNNING with its acting turn", stopped.Status, stopped.Acting != nil)
	}
	t.logf("✓ the invocation stopped mid-refund: the vendor has the refund, the run is RUNNING with its acting turn and no result")

	// The door sends the same message again (an answer lost on the way).
	again, err := crashing.Start(ctx, start)
	if !errors.Is(err, runs.ErrExists) || again.ID != r.ID {
		return fmt.Errorf("the door's second send: run %s (%v), want the first run %s", again.ID, err, r.ID)
	}
	if list, _ := store.List(ctx, t.tenant, tafID, 0); countSource(list, "email") != 1 {
		return fmt.Errorf("the door's second send: %d door runs, want 1", countSource(list, "email"))
	}
	t.logf("✓ the door's second send started no second run (the same run %s)", r.ID)

	// The next invocation: its lease is past, the sweep finds the run, and,
	// after the restart wait, continues it. Its clock is moved, not waited.
	var ahead atomic.Int64
	ahead.Store(int64(runner.MaxRun + runs.LeaseGrace + time.Second))
	now := func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) }
	fresh := *t.rn
	fresh.Runs, fresh.Now, fresh.Settle = store, now, kiff.Settle
	sw := runs.Sweeper{Runs: store, Ended: fresh.Ended, Hold: fresh.Held, Note: store.Again,
		Continue: func(ctx context.Context, tenant, id string) error {
			_, err := fresh.Execute(ctx, tenant, id, nil)
			return err
		}}
	if n, err := sw.Sweep(ctx, now(), 10); err != nil || n != 1 {
		return fmt.Errorf("the sweep after the stop: %d runs (%v), want 1", n, err)
	}
	ahead.Add(int64(runs.ResumeWaits[0] + time.Second))
	if _, err := sw.Sweep(ctx, now(), 10); err != nil {
		return fmt.Errorf("the sweep after the restart wait: %w", err)
	}
	done, err := store.Get(ctx, t.tenant, r.ID)
	if err != nil {
		return err
	}
	if done.Status != runs.Completed || done.Resumes != 1 {
		return fmt.Errorf("the carried-on run ended %s after %d restarts (%s), want COMPLETED after 1", done.Status, done.Resumes, done.Error)
	}
	t.logf("✓ a fresh invocation carried the run on from its last safe step: COMPLETED → %s", oneLine(done.Output))

	calls, err := t.vendorCalls(ctx)
	if err != nil {
		return err
	}
	refunds := 0
	for _, c := range calls {
		if strings.Contains(c, charge) && strings.Contains(c, "refund") {
			refunds++
		}
	}
	if refunds != 1 {
		return fmt.Errorf("the vendor has %d refunds of %s, want exactly 1", refunds, charge)
	}
	t.logf("✓ exactly one refund of %s reached the vendor", charge)

	mu.Lock()
	ended := 0
	for _, n := range notes {
		if n.Status.Final() {
			ended++
		}
	}
	all := len(notes)
	mu.Unlock()
	if ended != 1 {
		return fmt.Errorf("the door's receiver got %d end notes (of %d notes), want exactly 1: one reply", ended, all)
	}
	t.logf("✓ the door's receiver got exactly one end note, so exactly one reply (%d notes in all, the restart's pause included)", all)
	return nil
}

// stopsAfterSend is a tool of an invocation that stops mid-refund: the refund
// of charge goes to the vendor as usual, then its result never comes back
// (until stuck closes, when the test ends), as when a Lambda stops after
// KIFF sent the call.
type stopsAfterSend struct {
	tool.Tool
	charge string
	sent   func()
	stuck  chan struct{}
}

func (s stopsAfterSend) Call(ctx context.Context, input json.RawMessage) (tool.Result, error) {
	res, err := s.Tool.Call(ctx, input)
	if strings.Contains(strings.ToLower(s.Spec().Name), "refund") && strings.Contains(string(input), s.charge) {
		s.sent()
		<-s.stuck
	}
	return res, err
}

// countSource counts the runs one source started.
func countSource(list []runs.Run, source string) int {
	n := 0
	for _, r := range list {
		if r.Source == source {
			n++
		}
	}
	return n
}
