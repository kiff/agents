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
