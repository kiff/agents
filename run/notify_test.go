package run_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

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
	if len(n.got) != 2 || n.got[0] != want[0] || n.got[1] != want[1] {
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
