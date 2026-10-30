package run_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/store/memory"
)

// notes is a Notify URL that keeps what it was sent.
type notes struct {
	mu   sync.Mutex
	got  []run.Note
	code int
}

func (n *notes) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var note run.Note
	_ = json.NewDecoder(r.Body).Decode(&note)
	n.mu.Lock()
	n.got = append(n.got, note)
	n.mu.Unlock()
	if n.code != 0 {
		w.WriteHeader(n.code)
	}
}

// A run with a Notify is announced once at each pause and once at its end,
// whatever moves it; a run without one, and the moves in between, never.
func TestARunIsAnnouncedAtEachPauseAndAtItsEnd(t *testing.T) {
	ctx := context.Background()
	n := &notes{}
	srv := httptest.NewServer(n)
	defer srv.Close()
	runs := run.Notifying{Runs: memory.NewRuns()}
	now := time.Now()

	r := run.New("t1", "refunds", "s1", run.ByOwner, "go", now)
	r.Notify = run.Notify{URL: srv.URL, Ref: "ref-0123456789abcdef"}
	quiet := run.New("t1", "refunds", "s2", run.ByOwner, "go", now)
	for _, x := range []run.Run{r, quiet} {
		if err := runs.Create(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []run.Status{run.Running, run.Running, run.Paused, run.Paused, run.Running, run.Completed} {
		var err error
		if r, err = run.Change(ctx, runs, r, now, func(x *run.Run) error { x.Status = s; return nil }); err != nil {
			t.Fatal(err)
		}
		if quiet, err = run.Change(ctx, runs, quiet, now, func(x *run.Run) error { x.Status = s; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	want := []run.Note{
		{Run: r.ID, Agent: "refunds", Status: run.Paused, Ref: "ref-0123456789abcdef"},
		{Run: r.ID, Agent: "refunds", Status: run.Completed, Ref: "ref-0123456789abcdef"},
	}
	if !reflect.DeepEqual(n.got, want) {
		t.Fatalf("notes %+v", n.got)
	}
}

// A note that fails, or follows a redirect, never fails the run's change.
func TestANoteThatFailsNeverFailsTheChange(t *testing.T) {
	ctx := context.Background()
	other := &notes{}
	otherSrv := httptest.NewServer(other)
	defer otherSrv.Close()
	redirect := httptest.NewServer(http.RedirectHandler(otherSrv.URL, http.StatusTemporaryRedirect))
	defer redirect.Close()
	var logged int
	runs := run.Notifying{Runs: memory.NewRuns(), Log: func(string, error) { logged++ }}
	r := run.New("t1", "refunds", "s1", run.ByOwner, "go", time.Now())
	r.Notify = run.Notify{URL: redirect.URL, Ref: "ref-0123456789abcdef"}
	if err := runs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	r, err := run.Change(ctx, runs, r, time.Now(), func(x *run.Run) error { x.Status = run.Cancelled; return nil })
	if err != nil || r.Status != run.Cancelled {
		t.Fatal(r.Status, err)
	}
	if logged != 1 || len(other.got) != 0 {
		t.Fatalf("logged %d, the redirect's target got %d", logged, len(other.got))
	}
}

// Where a run is announced is fixed when it starts.
func TestNotifyNeverChanges(t *testing.T) {
	ctx := context.Background()
	runs := memory.NewRuns()
	r := run.New("t1", "refunds", "s1", run.ByOwner, "go", time.Now())
	if err := runs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := run.Change(ctx, runs, r, time.Now(), func(x *run.Run) error {
		x.Notify = run.Notify{URL: "https://elsewhere.test/", Ref: "ref-0123456789abcdef"}
		return nil
	}); err == nil {
		t.Fatal("a run's Notify changed")
	}
}

// A paused run's note names the cards it waits on, unanswered, so a service
// that answers some of them knows which run to read back; an ended run's
// names none.
func TestAPausedRunsNoteNamesTheCardsItWaitsOn(t *testing.T) {
	ctx := context.Background()
	n := &notes{}
	srv := httptest.NewServer(n)
	defer srv.Close()
	runs := run.Notifying{Runs: memory.NewRuns()}
	now := time.Now()
	r := run.New("t1", "setup", "s1", run.ByOwner, "go", now)
	r.Notify = run.Notify{URL: srv.URL, Ref: "ref-0123456789abcdef"}
	if err := runs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	card := func(id, typ string) run.Requirement {
		return run.Requirement{CallID: "c-" + id, Tool: "show", Kind: run.KindCard, Input: json.RawMessage(`{"type":"` + typ + `","id":"` + id + `"}`)}
	}
	answered := card("which", "confirm")
	answered.Answered = now
	r, err := run.Change(ctx, runs, r, now, func(x *run.Run) error { x.Status = run.Running; return nil })
	if err != nil {
		t.Fatal(err)
	}
	r, err = run.Change(ctx, runs, r, now, func(x *run.Run) error {
		x.Status, x.Paused = run.Paused, &run.Turn{}
		x.Requirements = []run.Requirement{answered, card("done", "result"), {CallID: "h1", Tool: "refund", Operation: "op"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.Change(ctx, runs, r, now, func(x *run.Run) error { x.Status = run.Cancelled; return nil }); err != nil {
		t.Fatal(err)
	}
	want := []run.Note{
		{Run: r.ID, Agent: "setup", Status: run.Paused, Ref: "ref-0123456789abcdef", Cards: []run.NoteCard{{ID: "done", Type: "result"}}},
		{Run: r.ID, Agent: "setup", Status: run.Cancelled, Ref: "ref-0123456789abcdef"},
	}
	if !reflect.DeepEqual(n.got, want) {
		t.Fatalf("notes %+v", n.got)
	}
}

// A crashed run's note says it's retrying, which restart of how many, and
// when; one past its last restart says it needs the owner, and why, so an
// app's line can say so rather than a bare ERROR.
func TestACrashedRunsNoteSaysWhereItStands(t *testing.T) {
	ctx := context.Background()
	n := &notes{}
	srv := httptest.NewServer(n)
	defer srv.Close()
	runs := run.Notifying{Runs: memory.NewRuns()}
	now := time.Now().UTC().Truncate(time.Second)
	acting := &run.Turn{Reply: model.Message{Role: model.Assistant, Blocks: []model.Block{{Kind: model.ToolUse, CallID: "c1", Tool: "refund"}}}}
	crashed := func(resumes int) run.Run {
		r := run.New("t1", "refunds", "s1", run.ByOwner, "go", now)
		r.Notify = run.Notify{URL: srv.URL, Ref: "ref-0123456789abcdef"}
		if err := runs.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		r, err := run.Claim(ctx, runs, "t1", r.ID, "i1", now, now)
		if err != nil {
			t.Fatal(err)
		}
		r, err = run.Change(ctx, runs, r, now, func(x *run.Run) error { x.Acting, x.Resumes = acting, resumes; return nil })
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	sw := run.Sweeper{Runs: runs, Hold: func(context.Context, run.Run) error { return nil },
		Continue: func(context.Context, string, string) error { return nil }, Ended: func(context.Context, run.Run) error { return nil }}
	first := crashed(0)
	last := crashed(run.MaxResumes)
	at := now.Add(time.Hour)
	if _, err := sw.Sweep(ctx, at, 10); err != nil {
		t.Fatal(err)
	}
	byRun := map[string]run.Note{}
	for _, note := range n.got {
		byRun[note.Run] = note
	}
	if got, want := byRun[first.ID], (run.Note{Run: first.ID, Agent: "refunds", Status: run.Paused, Ref: "ref-0123456789abcdef",
		State: "retrying", Reason: run.Restarted, Try: 1, Of: run.MaxResumes, NextAt: at.Add(run.ResumeWaits[0])}); !reflect.DeepEqual(got, want) {
		t.Fatalf("retrying note %+v, want %+v", got, want)
	}
	if got, want := byRun[last.ID], (run.Note{Run: last.ID, Agent: "refunds", Status: run.Error, Ref: "ref-0123456789abcdef",
		State: "needs_you", Reason: run.NeedsYou}); !reflect.DeepEqual(got, want) {
		t.Fatalf("needs_you note %+v, want %+v", got, want)
	}
}

// A note Notify doesn't take is kept on the run and sent again by the sweep,
// after each of NoteWaits, as the run is then; once taken it's no longer kept,
// and past the last try it's given up. The run's change never fails for it.
func TestANoteNotTakenIsKeptAndSentAgainUntilTaken(t *testing.T) {
	ctx := context.Background()
	n := &notes{code: http.StatusServiceUnavailable}
	srv := httptest.NewServer(n)
	defer srv.Close()
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store := memory.NewRunsAt(clock)
	runs := run.Notifying{Runs: store, Now: clock}
	sw := run.Sweeper{Runs: runs, Note: runs.Again}

	r := run.New("t1", "refunds", "s1", run.ByOwner, "go", now)
	r.Notify = run.Notify{URL: srv.URL, Ref: "ref-0123456789abcdef"}
	if err := runs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	r, err := run.Change(ctx, runs, r, now, func(x *run.Run) error { x.Status = run.Running; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.Change(ctx, runs, r, now, func(x *run.Run) error { x.Status = run.Completed; return nil }); err != nil {
		t.Fatal(err, "a note not taken never fails the change")
	}
	get := func() run.Run {
		got, err := store.Get(ctx, "t1", r.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := get(); got.NoteTries != 1 || !got.NoteRetry.Equal(now.Add(run.NoteWaits[0])) || !got.Due().Equal(got.NoteRetry) {
		t.Fatalf("kept as tries %d, retry %v, due %v", got.NoteTries, got.NoteRetry, got.Due())
	}

	// Not yet: nothing is sent before its time.
	if done, err := sw.Sweep(ctx, now.Add(run.NoteWaits[0]-time.Second), 10); err != nil || done != 0 || len(n.got) != 1 {
		t.Fatalf("swept %d early (%v), %d notes", done, err, len(n.got))
	}
	// Still not taken: kept for its next try, on the next wait.
	now = now.Add(run.NoteWaits[0] + time.Second)
	if _, err := sw.Sweep(ctx, now, 10); err != nil {
		t.Fatal(err)
	}
	if got := get(); len(n.got) != 2 || got.NoteTries != 2 || !got.NoteRetry.Equal(now.Add(run.NoteWaits[1])) {
		t.Fatalf("%d notes; tries %d, retry %v", len(n.got), got.NoteTries, got.NoteRetry)
	}
	// Taken: no longer kept, and never sent again.
	n.code = 0
	now = now.Add(run.NoteWaits[1] + time.Second)
	if _, err := sw.Sweep(ctx, now, 10); err != nil {
		t.Fatal(err)
	}
	if got := get(); len(n.got) != 3 || got.NoteTries != 0 || !got.NoteRetry.IsZero() || !got.Due().IsZero() {
		t.Fatalf("%d notes; tries %d, retry %v, due %v", len(n.got), got.NoteTries, got.NoteRetry, got.Due())
	}
	if last := n.got[2]; last.Status != run.Completed || last.Run != r.ID {
		t.Fatalf("sent again as %+v", last)
	}
	now = now.Add(2 * time.Hour)
	if _, err := sw.Sweep(ctx, now, 10); err != nil || len(n.got) != 3 {
		t.Fatalf("sent again after it was taken: %d (%v)", len(n.got), err)
	}
}

// A note never taken is given up after its last try: the run isn't swept for
// it again.
func TestANoteIsGivenUpAfterItsLastTry(t *testing.T) {
	ctx := context.Background()
	n := &notes{code: http.StatusBadGateway}
	srv := httptest.NewServer(n)
	defer srv.Close()
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store := memory.NewRunsAt(clock)
	var logged []string
	runs := run.Notifying{Runs: store, Now: clock, Log: func(msg string, _ error) { logged = append(logged, msg) }}
	sw := run.Sweeper{Runs: runs, Note: runs.Again}
	r := run.New("t1", "refunds", "s1", run.ByOwner, "go", now)
	r.Notify = run.Notify{URL: srv.URL, Ref: "ref-0123456789abcdef"}
	if err := runs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := run.Change(ctx, runs, r, now, func(x *run.Run) error { x.Status = run.Cancelled; return nil }); err != nil {
		t.Fatal(err)
	}
	for range run.NoteWaits {
		now = now.Add(2 * time.Hour)
		if _, err := sw.Sweep(ctx, now, 10); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.Get(ctx, "t1", r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(n.got) != 1+len(run.NoteWaits) || got.NoteTries != 0 || !got.Due().IsZero() {
		t.Fatalf("%d notes; tries %d, due %v", len(n.got), got.NoteTries, got.Due())
	}
	if logged[len(logged)-1] != "run: a note was given up after its last try" {
		t.Fatalf("logged %q", logged)
	}
}

// A note kept for a pause is dropped when the run moves on: the run is noted
// for its new status on its own, never for the one it left.
func TestAKeptNoteGoesWithTheStatusItWasFor(t *testing.T) {
	ctx := context.Background()
	n := &notes{code: http.StatusServiceUnavailable}
	srv := httptest.NewServer(n)
	defer srv.Close()
	now := time.Now().UTC()
	store := memory.NewRuns()
	runs := run.Notifying{Runs: store}
	r := run.New("t1", "refunds", "s1", run.ByOwner, "go", now)
	r.Notify = run.Notify{URL: srv.URL, Ref: "ref-0123456789abcdef"}
	if err := runs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	var err error
	for _, s := range []run.Status{run.Running, run.Paused} {
		if r, err = run.Change(ctx, runs, r, now, func(x *run.Run) error { x.Status = s; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if r, err = store.Get(ctx, "t1", r.ID); err != nil || r.NoteTries != 1 {
		t.Fatalf("tries %d, %v", r.NoteTries, err)
	}
	if r, err = run.Change(ctx, runs, r, now, func(x *run.Run) error { x.Status = run.Running; return nil }); err != nil {
		t.Fatal(err)
	}
	if r.NoteTries != 0 || !r.NoteRetry.IsZero() {
		t.Fatalf("still kept at work: tries %d, retry %v", r.NoteTries, r.NoteRetry)
	}
}
