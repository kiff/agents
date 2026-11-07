package run_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/store/memory"
)

// reports is KIFF's usage route as the tests see it: it keeps the runs it
// was sent, and fails while fail is set.
type reports struct {
	mu   sync.Mutex
	got  []run.Run
	fail bool
}

func (k *reports) send(_ context.Context, r run.Run) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.fail {
		return errors.New("kiff: usage report: 503 Service Unavailable")
	}
	k.got = append(k.got, r)
	return nil
}

func (k *reports) sent() []run.Run {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]run.Run(nil), k.got...)
}

// moveTo changes r to status s with cost spent so far.
func moveTo(t *testing.T, runs run.Runs, r run.Run, now time.Time, s run.Status, cost int64) run.Run {
	t.Helper()
	r, err := run.Change(context.Background(), runs, r, now, func(x *run.Run) error { x.Status, x.Cost = s, cost; return nil })
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A run that ends having used AI is reported once, as it was stored, on the
// move that ends it, whatever made it; its pauses and later changes aren't,
// and a run that used no AI never is.
func TestARunIsReportedOnceWhenItEndsHavingUsedAI(t *testing.T) {
	ctx := context.Background()
	k := &reports{}
	runs := run.Reporting{Runs: memory.NewRuns(), Report: k.send}
	now := time.Now().UTC().Truncate(time.Second)

	r := run.New("t1", "refunds", "s1", run.ByOwner, "go", now)
	idle := run.New("t1", "refunds", "s2", run.BySchedule, "go", now)
	for _, x := range []run.Run{r, idle} {
		if err := runs.Create(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	r = moveTo(t, runs, r, now, run.Running, 0)
	r = moveTo(t, runs, r, now.Add(time.Minute), run.Paused, 12_000)
	r = moveTo(t, runs, r, now.Add(2*time.Minute), run.Running, 12_000)
	if len(k.sent()) != 0 {
		t.Fatalf("reported before it ended: %+v", k.sent())
	}
	end := now.Add(3 * time.Minute)
	r = moveTo(t, runs, r, end, run.Completed, 30_000)
	if !r.Ended.Equal(end) {
		t.Fatalf("ended at %v, want %v", r.Ended, end)
	}
	// A write once it has ended (its note kept, say) neither reports it
	// again nor moves its end.
	later := r
	later.NoteTries, later.Version, later.Updated = 1, r.Version+1, end.Add(time.Hour)
	if err := runs.Put(ctx, r, later); err != nil {
		t.Fatal(err)
	}
	if r, _ = runs.Get(ctx, "t1", r.ID); !r.Ended.Equal(end) || !r.Updated.Equal(end.Add(time.Hour)) {
		t.Fatalf("a later write: ended %v, updated %v", r.Ended, r.Updated)
	}
	got := k.sent()
	if len(got) != 1 || got[0].ID != r.ID || got[0].Cost != 30_000 || got[0].Status != run.Completed || !got[0].Ended.Equal(end) {
		t.Fatalf("reported %+v, want the run once, as it ended", got)
	}
	if stored, _ := runs.Get(ctx, "t1", r.ID); stored.UsageTries != 0 || !stored.UsageRetry.IsZero() {
		t.Fatalf("a report KIFF took is still owed: %d, %v", stored.UsageTries, stored.UsageRetry)
	}

	// Refused before its first turn: no AI, no report.
	idle = moveTo(t, runs, idle, now, run.Error, 0)
	if len(k.sent()) != 1 {
		t.Fatalf("a run that used no AI was reported: %+v", k.sent()[1:])
	}
	if !run.Reported(r) || run.Reported(idle) {
		t.Fatalf("Reported(%s, %d) and Reported(%s, %d)", r.Status, r.Cost, idle.Status, idle.Cost)
	}
}

// A report KIFF doesn't take never fails the run's change: it's kept on the
// run, which is due for the sweep at its next try, sent again until KIFF
// takes it, and given up after its last try.
func TestAReportNotTakenIsKeptAndSentAgainUntilTaken(t *testing.T) {
	ctx := context.Background()
	k := &reports{fail: true}
	var logged []string
	now := time.Now().UTC().Truncate(time.Second)
	clock := now
	runs := run.Reporting{Runs: memory.NewRuns(), Report: k.send, Now: func() time.Time { return clock },
		Log: func(msg string, _ error) { logged = append(logged, msg) }}
	r := run.New("t1", "refunds", "s1", run.ByOwner, "go", now)
	if err := runs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	r = moveTo(t, runs, r, now, run.Running, 0)
	r = moveTo(t, runs, r, now, run.Error, 8_000) // failed on our side, after a turn: still reported
	stored, _ := runs.Get(ctx, "t1", r.ID)
	if stored.UsageTries != 1 || !stored.UsageRetry.Equal(now.Add(run.UsageWaits[0])) || !stored.Due().Equal(stored.UsageRetry) {
		t.Fatalf("kept as %d, %v, due %v; want 1, %v", stored.UsageTries, stored.UsageRetry, stored.Due(), now.Add(run.UsageWaits[0]))
	}
	// Each try not taken waits longer, until the last.
	for try := 2; try <= len(run.UsageWaits); try++ {
		clock = clock.Add(run.UsageWaits[try-2])
		if err := runs.Again(ctx, stored); err != nil {
			t.Fatal(err)
		}
		stored, _ = runs.Get(ctx, "t1", r.ID)
		if stored.UsageTries != try || !stored.UsageRetry.Equal(clock.Add(run.UsageWaits[try-1])) {
			t.Fatalf("try %d kept as %d, %v", try, stored.UsageTries, stored.UsageRetry)
		}
	}
	// Taken at last: no longer owed, and no longer due.
	k.fail = false
	if err := runs.Again(ctx, stored); err != nil {
		t.Fatal(err)
	}
	stored, _ = runs.Get(ctx, "t1", r.ID)
	if got := k.sent(); len(got) != 1 || stored.UsageTries != 0 || !stored.UsageRetry.IsZero() || !stored.Due().IsZero() {
		t.Fatalf("taken: sent %d, kept as %d, %v, due %v", len(got), stored.UsageTries, stored.UsageRetry, stored.Due())
	}

	// One not taken on its last try is given up, and said.
	k.fail = true
	r2 := run.New("t1", "refunds", "s2", run.ByOwner, "go", now)
	if err := runs.Create(ctx, r2); err != nil {
		t.Fatal(err)
	}
	r2 = moveTo(t, runs, r2, now, run.Running, 0)
	r2 = moveTo(t, runs, r2, now, run.Completed, 5_000)
	r2, _ = runs.Get(ctx, "t1", r2.ID)
	r2.UsageTries = len(run.UsageWaits)
	logged = nil
	if err := runs.Again(ctx, r2); err != nil {
		t.Fatal(err)
	}
	if stored, _ := runs.Get(ctx, "t1", r2.ID); !stored.UsageRetry.IsZero() || len(logged) != 1 || logged[0] != "run: a usage report was given up after its last try" {
		t.Fatalf("past its last try: retry %v, logged %q", stored.UsageRetry, logged)
	}
}

// A run's note and its usage report both kept on the move that ends it: one
// keeping doesn't lose the other, though each was read before the other's.
func TestAKeptReportAndAKeptNoteBothStay(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer srv.Close()
	k := &reports{fail: true}
	now := time.Now().UTC().Truncate(time.Second)
	clock := func() time.Time { return now }
	runs := run.Reporting{Runs: run.Notifying{Runs: memory.NewRuns(), Now: clock}, Report: k.send, Now: clock}
	r := run.New("t1", "refunds", "s1", run.ByOwner, "go", now)
	r.Notify = run.Notify{URL: srv.URL, Ref: "ref-0123456789abcdef"}
	if err := runs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	r = moveTo(t, runs, r, now, run.Running, 0)
	r = moveTo(t, runs, r, now, run.Completed, 9_000)
	stored, _ := runs.Get(ctx, "t1", r.ID)
	if stored.NoteTries != 1 || stored.UsageTries != 1 || stored.NoteRetry.IsZero() || stored.UsageRetry.IsZero() {
		t.Fatalf("kept: note %d %v, report %d %v; want both", stored.NoteTries, stored.NoteRetry, stored.UsageTries, stored.UsageRetry)
	}
}

// The sweep sends a kept report again once its time has come, and not before.
func TestTheSweepSendsAKeptReportAgain(t *testing.T) {
	ctx := context.Background()
	k := &reports{fail: true}
	now := time.Now().UTC().Truncate(time.Second)
	runs := run.Reporting{Runs: memory.NewRuns(), Report: k.send, Now: func() time.Time { return now }}
	r := run.New("t1", "refunds", "s1", run.ByOwner, "go", now)
	if err := runs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	r = moveTo(t, runs, r, now, run.Running, 0)
	r = moveTo(t, runs, r, now, run.Completed, 9_000)
	k.fail = false
	sw := run.Sweeper{Runs: runs, Usage: runs.Again}
	if n, err := sw.Sweep(ctx, now.Add(run.UsageWaits[0]-time.Second), 10); err != nil || n != 0 || len(k.sent()) != 0 {
		t.Fatalf("before its time: %d, %v, sent %d", n, err, len(k.sent()))
	}
	if n, err := sw.Sweep(ctx, now.Add(run.UsageWaits[0]+time.Second), 10); err != nil || n != 1 || len(k.sent()) != 1 {
		t.Fatalf("at its time: %d, %v, sent %d", n, err, len(k.sent()))
	}
	if stored, _ := runs.Get(ctx, "t1", r.ID); !stored.UsageRetry.IsZero() || stored.Status != run.Completed {
		t.Fatalf("after the sweep: %+v", stored)
	}
}
