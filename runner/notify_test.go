package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/tool"
)

// conflicts is a Runs that counts the writes it refused as changed meanwhile.
type conflicts struct {
	run.Runs
	mu sync.Mutex
	n  int
}

func (c *conflicts) Put(ctx context.Context, prev, next run.Run) error {
	err := c.Runs.Put(ctx, prev, next)
	if errors.Is(err, run.ErrConflict) {
		c.mu.Lock()
		c.n++
		c.mu.Unlock()
	}
	return err
}

func (c *conflicts) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// A run started with notify pauses, has its hold answered (as KIFF's alert
// records it) and is cancelled through the API, on the runs as production
// wires them (Reporting over Notifying): the note each noted move owes is
// written with it and settled after it, and no write the pause, the answer
// or the cancel makes loses to it. Each is announced once, and nothing is
// owed once it's taken.
func TestANotedRunIsAnsweredAndCancelledWithoutAConflict(t *testing.T) {
	f := newFixture(t, use("c1", "stripe_refund", `{"order":42}`), say("Waiting for approval."))
	var mu sync.Mutex
	var notes []run.Note
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n run.Note
		_ = json.NewDecoder(r.Body).Decode(&n)
		mu.Lock()
		notes = append(notes, n)
		mu.Unlock()
	}))
	defer receiver.Close()
	counted := &conflicts{Runs: f.runs}
	runs := run.Reporting{Runs: run.Notifying{Runs: counted, Client: receiver.Client(), Now: f.clock.now},
		Report: func(context.Context, run.Run) error { return nil }, Now: f.clock.now}
	f.r.Runs = runs
	f.r.Collect = func(context.Context, agent.Agent, []run.Requirement) ([]agent.ToolResultBody, error) {
		return []agent.ToolResultBody{{CallID: "c1", Tool: "stripe_refund", Result: tool.Result{Text: "refunded re_1"}}}, nil
	}
	var withdrawn []string
	srv := httptest.NewServer((&server.Server{
		Auth:   func(*http.Request) (server.Caller, error) { return server.Caller{Tenant: "t1"}, nil },
		Agents: f.agents, Sessions: f.store, Notes: f.store, Runs: runs, Runner: f.r, Now: f.clock.now,
		// KIFF holds nothing more once the hold is answered.
		Withdraw: func(_ context.Context, _ run.Run, ex string) (string, error) {
			withdrawn = append(withdrawn, ex)
			return "", nil
		},
		Background: func(ctx context.Context, tenant, id string) error {
			_, err := f.r.Execute(ctx, tenant, id, nil)
			return err
		},
	}).Handler())
	defer srv.Close()

	r := f.start(server.Start{Input: "refund order 42", Notify: run.Notify{URL: receiver.URL, Ref: "ref-0123456789abcdef"}})
	f.refund.result = tool.Result{Text: "KIFF holds this call for the owner", State: tool.StateHeld,
		Hold: &tool.Hold{Operation: "op_1", Exception: "exc_1", Expires: f.clock.now().Add(10 * time.Minute)}}
	paused, err := f.r.Execute(ctx, "t1", r.ID, nil)
	if err != nil || paused.Status != run.Paused {
		t.Fatalf("paused: %s, %v", paused.Status, err)
	}
	owesNothing := func(when string) {
		t.Helper()
		got := f.get(r.ID)
		if got.NoteTries != 0 || !got.NoteRetry.IsZero() {
			t.Fatalf("%s: a note is still owed (tries %d, at %v)", when, got.NoteTries, got.NoteRetry)
		}
	}
	if n := counted.count(); n != 0 {
		t.Fatalf("pausing a noted run: %d writes lost to a conflict", n)
	}
	owesNothing("after the pause")

	// KIFF's alert records the answer, as its handler does.
	f.clock.add(time.Second)
	if _, err := run.Answer(ctx, runs, "t1", "exc_1", "approved", f.clock.now()); err != nil {
		t.Fatal(err)
	}
	if n := counted.count(); n != 0 {
		t.Fatalf("answering a noted pause: %d writes lost to a conflict", n)
	}
	owesNothing("after the answer")

	// The owner cancels it through the API: the cancel is recorded, and the
	// resolved run is handed on, continued and ended CANCELLED.
	f.clock.add(time.Second)
	resp, err := http.Post(srv.URL+"/agents/rae/runs/"+r.ID+"/cancel", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel: %d %s", resp.StatusCode, body)
	}
	if n := counted.count(); n != 0 {
		t.Fatalf("cancelling a noted pause: %d writes lost to a conflict", n)
	}
	got := f.get(r.ID)
	if got.Status != run.Cancelled || len(withdrawn) != 1 {
		t.Fatalf("after the cancel: %s, withdrew %v; want CANCELLED, exc_1 asked once", got.Status, withdrawn)
	}
	owesNothing("after the cancel")
	if !got.Due().IsZero() {
		t.Fatalf("still due at %v", got.Due())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(notes) != 2 || notes[0].Status != run.Paused || notes[1].Status != run.Cancelled {
		t.Fatalf("notes %+v; want the pause, then the cancel", notes)
	}
}
