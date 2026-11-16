package run_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/store/memory"
)

// crash is what a faulty store or client panics with: the invocation stops
// there, and nothing after it runs.
type crash struct{}

// crashes runs f, and reports whether it stopped on a crash.
func crashes(f func()) (crashed bool) {
	defer func() {
		if p := recover(); p != nil {
			if _, ok := p.(crash); !ok {
				panic(p)
			}
			crashed = true
		}
	}()
	f()
	return false
}

// faulty is a Runs whose writes can fail before they're stored (fail), or
// crash the invocation once they are (crashAfter).
type faulty struct {
	run.Runs
	mu         sync.Mutex
	fail       func(prev, next run.Run) error
	crashAfter func(prev, next run.Run) bool
}

func (f *faulty) Put(ctx context.Context, prev, next run.Run) error {
	f.mu.Lock()
	fail, after := f.fail, f.crashAfter
	f.mu.Unlock()
	if fail != nil {
		if err := fail(prev, next); err != nil {
			return err
		}
	}
	if err := f.Runs.Put(ctx, prev, next); err != nil {
		return err
	}
	if after != nil && after(prev, next) {
		panic(crash{})
	}
	return nil
}

// heal makes f a store that writes everything.
func (f *faulty) heal() {
	f.mu.Lock()
	f.fail, f.crashAfter = nil, nil
	f.mu.Unlock()
}

// ending is a write that moves a run to a final status.
func ending(prev, next run.Run) bool { return !prev.Status.Final() && next.Status.Final() }

// business counts the sweep's hooks that would carry a run's work on: a note
// sent again must never call any of them for a run that has ended.
type business struct{ continued, held, ended int }

func (b *business) sweeper(runs run.Runs, note func(context.Context, run.Run) error) run.Sweeper {
	return run.Sweeper{Runs: runs, Note: note,
		Continue: func(context.Context, string, string) error { b.continued++; return nil },
		Hold:     func(context.Context, run.Run) error { b.held++; return nil },
		Ended:    func(context.Context, run.Run) error { b.ended++; return nil }}
}

// A run's end, once stored, is announced even when the invocation that
// stored it stops before it sends the note: the note was owed in the same
// write as the end, and the sweep sends it. Each tenant's run is announced
// to its own Notify, with its own ref, once.
func TestACrashAfterTheEndIsStoredNeverLosesItsNote(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store := memory.NewRunsAt(clock)
	f := &faulty{Runs: store, crashAfter: ending}
	crashing := run.Notifying{Runs: f, Now: clock}

	type tenant struct {
		id, ref string
		got     *notes
		r       run.Run
	}
	tenants := []*tenant{{id: "t1", ref: "ref-t1-0123456789abcdef", got: &notes{}}, {id: "t2", ref: "ref-t2-0123456789abcdef", got: &notes{}}}
	for _, tn := range tenants {
		srv := httptest.NewServer(tn.got)
		defer srv.Close()
		tn.r = run.New(tn.id, "refunds", "s1", run.ByOwner, "go", now)
		tn.r.Notify = run.Notify{URL: srv.URL, Ref: tn.ref}
		if err := crashing.Create(ctx, tn.r); err != nil {
			t.Fatal(err)
		}
		r, err := run.Change(ctx, crashing, tn.r, now, func(x *run.Run) error { x.Status = run.Running; return nil })
		if err != nil {
			t.Fatal(err)
		}
		if !crashes(func() {
			_, _ = run.Change(ctx, crashing, r, now, func(x *run.Run) error { x.Status, x.Output = run.Completed, "refunded"; return nil })
		}) {
			t.Fatal("the invocation didn't stop after storing the end")
		}
		if got, err := store.Get(ctx, tn.id, r.ID); err != nil || got.Status != run.Completed || len(tn.got.got) != 0 {
			t.Fatalf("after the crash: %s (%v), %d notes; want COMPLETED stored and no note sent", got.Status, err, len(tn.got.got))
		}
	}

	// A fresh invocation's sweep, a while later.
	f.heal()
	runs := run.Notifying{Runs: store, Now: clock}
	var b business
	sw := b.sweeper(runs, runs.Again)
	now = now.Add(run.NoteWaits[0] + time.Second)
	if _, err := sw.Sweep(ctx, now, 10); err != nil {
		t.Fatal(err)
	}
	for _, tn := range tenants {
		want := []run.Note{{Run: tn.r.ID, Agent: "refunds", Status: run.Completed, Ref: tn.ref}}
		if !reflect.DeepEqual(tn.got.got, want) {
			t.Fatalf("tenant %s: the note was lost after the crash: got %+v, want %+v", tn.id, tn.got.got, want)
		}
		if got, _ := store.Get(ctx, tn.id, tn.r.ID); !got.NoteRetry.IsZero() || !got.Due().IsZero() {
			t.Fatalf("tenant %s: still owed after it was taken: retry %v, due %v", tn.id, got.NoteRetry, got.Due())
		}
	}
	if b != (business{}) {
		t.Fatalf("the sweep carried an ended run's work on: %+v", b)
	}
}

// A note Notify didn't take whose retry state couldn't be stored (the store
// failed, or lost the write to another) is still sent again by the sweep,
// as the run is: the run isn't continued, held or ended again, and nothing
// it did is done twice.
func TestAFailedNoteIsRecoveredWithoutRerunningTheRun(t *testing.T) {
	for name, fault := range map[string]error{
		"the store fails":            errors.New("store: throttled"),
		"the write loses to another": run.ErrConflict,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			n := &notes{code: http.StatusServiceUnavailable}
			srv := httptest.NewServer(n)
			defer srv.Close()
			now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
			clock := func() time.Time { return now }
			store := memory.NewRunsAt(clock)
			// Every write after the end fails: the note's retry state among them.
			f := &faulty{Runs: store, fail: func(prev, next run.Run) error {
				if prev.Status.Final() {
					return fault
				}
				return nil
			}}
			runs := run.Notifying{Runs: f, Now: clock}
			r := run.New("t1", "refunds", "s1", run.ByOwner, "go", now)
			r.Notify = run.Notify{URL: srv.URL, Ref: "ref-0123456789abcdef"}
			if err := runs.Create(ctx, r); err != nil {
				t.Fatal(err)
			}
			r, err := run.Change(ctx, runs, r, now, func(x *run.Run) error { x.Status = run.Running; return nil })
			if err != nil {
				t.Fatal(err)
			}
			done, err := run.Change(ctx, runs, r, now, func(x *run.Run) error {
				x.Status, x.Output, x.Done = run.Completed, "refunded", []run.Done{{CallID: "c1", Tool: "refund"}}
				return nil
			})
			if err != nil {
				t.Fatal(err, "a note not taken never fails the change")
			}
			if len(n.got) != 1 {
				t.Fatalf("%d notes on the end, want 1 (not taken)", len(n.got))
			}

			f.heal()
			n.code = 0
			var b business
			sw := b.sweeper(runs, runs.Again)
			now = now.Add(run.NoteWaits[0] + time.Second)
			if _, err := sw.Sweep(ctx, now, 10); err != nil {
				t.Fatal(err)
			}
			if len(n.got) != 2 {
				t.Fatalf("the failed note was not recovered: %d notes sent, want 2 (the failed one, then the sweep's)", len(n.got))
			}
			if last, want := n.got[1], (run.Note{Run: r.ID, Agent: "refunds", Status: run.Completed, Ref: "ref-0123456789abcdef"}); !reflect.DeepEqual(last, want) {
				t.Fatalf("sent again as %+v, want %+v", last, want)
			}
			got, err := store.Get(ctx, "t1", r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if b != (business{}) || got.Status != run.Completed || got.Output != done.Output || !reflect.DeepEqual(got.Done, done.Done) || !got.Ended.Equal(done.Ended) {
				t.Fatalf("recovering the note reran the run: hooks %+v, run %s %q %+v", b, got.Status, got.Output, got.Done)
			}
			if !got.Due().IsZero() {
				t.Fatalf("still due after the note was taken: %v", got.Due())
			}
			// Taken: never sent again.
			now = now.Add(3 * time.Hour)
			if _, err := sw.Sweep(ctx, now, 10); err != nil || len(n.got) != 2 {
				t.Fatalf("sent again after it was taken: %d (%v)", len(n.got), err)
			}
		})
	}
}

// A note still owed when the run's usage report is kept (both failed on the
// move that ended it, and the note's own write failed too) stays owed: the
// report's write keeps it, and the sweep sends it.
func TestANoteOwedStaysOwedWhenTheUsageReportIsKept(t *testing.T) {
	ctx := context.Background()
	n := &notes{code: http.StatusServiceUnavailable}
	srv := httptest.NewServer(n)
	defer srv.Close()
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store := memory.NewRunsAt(clock)
	// The writes after the end fail, but the usage report's.
	f := &faulty{Runs: store, fail: func(prev, next run.Run) error {
		if prev.Status.Final() && next.UsageTries == prev.UsageTries {
			return errors.New("store: throttled")
		}
		return nil
	}}
	k := &reports{fail: true}
	runs := run.Reporting{Runs: run.Notifying{Runs: f, Now: clock}, Report: k.send, Now: clock}
	r := run.New("t1", "refunds", "s1", run.ByOwner, "go", now)
	r.Notify = run.Notify{URL: srv.URL, Ref: "ref-0123456789abcdef"}
	if err := runs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	r = moveTo(t, runs, r, now, run.Running, 0)
	r = moveTo(t, runs, r, now, run.Completed, 9_000)
	if got, _ := store.Get(ctx, "t1", r.ID); got.UsageTries != 1 || got.NoteRetry.IsZero() {
		t.Fatalf("after the end: report tries %d, note owed at %v; want both owed", got.UsageTries, got.NoteRetry)
	}

	f.heal()
	n.code, k.fail = 0, false
	notifying := runs.Runs.(run.Notifying)
	sw := run.Sweeper{Runs: runs, Note: notifying.Again, Usage: runs.Again}
	for range 3 {
		now = now.Add(run.NoteWaits[0] + time.Second)
		if _, err := sw.Sweep(ctx, now, 10); err != nil {
			t.Fatal(err)
		}
	}
	if len(n.got) != 2 || len(k.sent()) != 1 {
		t.Fatalf("%d notes, %d reports; want the note sent again and the report once", len(n.got), len(k.sent()))
	}
	if got, _ := store.Get(ctx, "t1", r.ID); !got.Due().IsZero() {
		t.Fatalf("still owed: due %v", got.Due())
	}
}

// A sweep that sends no notes leaves a run that owes one as it is: an
// ended or paused run due only for its note is never ended or continued
// for it.
func TestASweepWithoutNotesLeavesARunThatOwesOne(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store := memory.NewRunsAt(clock)
	f := &faulty{Runs: store, crashAfter: func(prev, next run.Run) bool { return prev.Status != next.Status && next.Status != run.Running }}
	runs := run.Notifying{Runs: f, Now: clock}
	var ids []string
	for _, s := range []run.Status{run.Completed, run.Paused} {
		r := run.New("t1", "refunds", "s-"+string(s), run.ByOwner, "go", now)
		r.Notify = run.Notify{URL: "http://127.0.0.1:1/", Ref: "ref-0123456789abcdef"}
		if err := runs.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		r = moveTo(t, runs, r, now, run.Running, 0)
		crashes(func() {
			_, _ = run.Change(ctx, runs, r, now, func(x *run.Run) error {
				x.Status = s
				if s == run.Paused {
					x.Paused, x.Requirements = &run.Turn{}, []run.Requirement{{CallID: "c1", Tool: "show", Kind: run.KindCard, Expires: now.Add(run.CardWait)}}
				}
				return nil
			})
		})
		ids = append(ids, r.ID)
	}
	var b business
	sw := b.sweeper(store, nil)
	now = now.Add(time.Hour)
	if n, err := sw.Sweep(ctx, now, 10); n != 0 || err != nil || b != (business{}) {
		t.Fatalf("swept %d (%v), hooks %+v; want none", n, err, b)
	}
	for _, id := range ids {
		if got, _ := store.Get(ctx, "t1", id); got.Status == run.Error || got.NoteRetry.IsZero() {
			t.Fatalf("%s: %s, owed at %v; want left as it was", id, got.Status, got.NoteRetry)
		}
	}
}

// dedup is a Notify URL that, as tafs cloud does, claims each note's
// delivery once by what the note names (its ref, its run and its status) and
// delivers it once, however many times the note comes. answer, when set, is
// what it answers after it took a note.
type dedup struct {
	mu        sync.Mutex
	posts     []run.Note
	delivered map[[3]string]int
	answer    int
}

func (d *dedup) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var note run.Note
	_ = json.NewDecoder(r.Body).Decode(&note)
	d.mu.Lock()
	d.posts = append(d.posts, note)
	d.delivered[[3]string{note.Ref, note.Run, string(note.Status)}]++
	answer := d.answer
	d.mu.Unlock()
	if answer != 0 {
		w.WriteHeader(answer)
	}
}

// crashAfterSend is a client transport that stops the invocation once
// Notify has answered, before the sender has recorded the answer.
type crashAfterSend struct{ on bool }

func (c *crashAfterSend) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err == nil && c.on {
		resp.Body.Close()
		panic(crash{})
	}
	return resp, err
}

// A note sent more than once (its answer was lost, or Notify took it and
// still answered an error) is the same note each time: the same run, ref
// and status, so a receiver that claims a delivery by them delivers the
// result once. Once a note is taken, the sender never sends it again.
func TestARepeatedNoteIsOneDelivery(t *testing.T) {
	for name, tc := range map[string]struct {
		answer int  // what Notify answers after taking the note, at first
		crash  bool // the invocation stops once Notify answered
	}{
		"the invocation stops after Notify took it": {crash: true},
		"Notify took it and answered an error":      {answer: http.StatusGatewayTimeout},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			d := &dedup{delivered: map[[3]string]int{}, answer: tc.answer}
			srv := httptest.NewServer(d)
			defer srv.Close()
			now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
			clock := func() time.Time { return now }
			store := memory.NewRunsAt(clock)
			transport := &crashAfterSend{on: tc.crash}
			runs := run.Notifying{Runs: store, Now: clock, Client: &http.Client{Transport: transport}}
			r := run.New("t1", "refunds", "s1", run.ByOwner, "go", now)
			r.Notify = run.Notify{URL: srv.URL, Ref: "ref-0123456789abcdef"}
			if err := runs.Create(ctx, r); err != nil {
				t.Fatal(err)
			}
			r, err := run.Change(ctx, runs, r, now, func(x *run.Run) error { x.Status = run.Running; return nil })
			if err != nil {
				t.Fatal(err)
			}
			crashed := crashes(func() {
				_, err = run.Change(ctx, runs, r, now, func(x *run.Run) error { x.Status, x.Output = run.Completed, "refunded"; return nil })
			})
			if crashed != tc.crash || (!crashed && err != nil) {
				t.Fatalf("crashed %v (%v), want %v", crashed, err, tc.crash)
			}

			transport.on = false
			d.mu.Lock()
			d.answer = 0
			d.mu.Unlock()
			var b business
			sw := b.sweeper(runs, runs.Again)
			for range 4 {
				now = now.Add(2 * time.Hour)
				if _, err := sw.Sweep(ctx, now, 10); err != nil {
					t.Fatal(err)
				}
			}
			want := run.Note{Run: r.ID, Agent: "refunds", Status: run.Completed, Ref: "ref-0123456789abcdef"}
			for i, p := range d.posts {
				if !reflect.DeepEqual(p, want) {
					t.Fatalf("post %d is %+v, want the same note %+v", i, p, want)
				}
			}
			// Sent again, since the sender can't know Notify took it, and
			// delivered once.
			key := [3]string{want.Ref, want.Run, string(want.Status)}
			if len(d.posts) != 2 || len(d.delivered) != 1 || d.delivered[key] != 2 {
				t.Fatalf("%d posts, %d logical deliveries (%v); want 2 posts of one note", len(d.posts), len(d.delivered), d.delivered)
			}
			if got, _ := store.Get(ctx, "t1", r.ID); !got.Due().IsZero() || b != (business{}) {
				t.Fatalf("still owed (due %v), or the run was carried on (%+v)", got.Due(), b)
			}
		})
	}
}
