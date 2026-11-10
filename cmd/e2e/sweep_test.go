package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeKIFF is the test account in KIFF as a phone's session sees it: the
// agents that aren't archived and which is Setup's (GET /v1/me/access), and
// archiving one by its id. An agent the prune deleted since the read is a
// 404. Any other route is refused as KIFF refuses a phone's session, and
// recorded: the sweep must ask for nothing else.
func fakeKIFF(t *testing.T, agents []string, deleted []string, failArchive bool) (*client, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/me/access":
			out := map[string]any{}
			for _, a := range agents {
				out[a] = map[string]any{"use": true, "manage": true}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"agents": out, "setup_agent": "e2e-setup"})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/me/agents/") && strings.HasSuffix(r.URL.Path, "/archive"):
			a := strings.Split(r.URL.Path, "/")[4]
			calls = append(calls, "archive "+a)
			switch {
			case failArchive:
				w.WriteHeader(http.StatusInternalServerError)
			case slices.Contains(deleted, a):
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"no agent record named ` + a + `"}`))
			default:
				agents = slices.DeleteFunc(agents, func(x string) bool { return x == a })
				_, _ = w.Write([]byte(`{"status":"archived"}`))
			}
		default:
			calls = append(calls, "refused "+r.Method+" "+r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"phone_session_forbidden","message":"a phone's session reads Cards and held calls and answers with its key's signature; do this in KIFF"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return &client{base: srv.URL, http: srv.Client(), auth: func(*http.Request, []byte) {}}, &calls
}

// The account as the sweep finds it: Setup's agent, a gateway agent, a taf
// this job made, and a taf from an earlier run.
func sweepAgents() []string { return []string{"e2e-setup", "gw", "taf_made", "taf_earlier"} }

// The sweep archives the tafs this job made that are still there, and
// nothing else: a taf already archived is left, Setup's agent is never
// taken even when -made names it, and no other agent is touched.
func TestTheSweepArchivesThisJobsTafsAndNothingElse(t *testing.T) {
	kiffc, calls := fakeKIFF(t, sweepAgents(), nil, false)
	c, err := sweep(context.Background(), kiffc, []string{"taf_made", "taf_archived_by_its_run", "e2e-setup"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"archive taf_made"}; !slices.Equal(*calls, want) {
		t.Fatalf("calls %v, want %v", *calls, want)
	}
	if c != (sweepCounts{archived: 1, gone: 1}) {
		t.Fatalf("counts %+v", c)
	}
	if s := c.String(); strings.Contains(s, "taf_") || strings.Contains(s, "e2e-") {
		t.Fatalf("the public log names an id: %s", s)
	}
}

// KIFF refuses a phone's session every route that isn't the phone's
// (listing the account's keys among them, 403). The sweep asks for none of
// them, so it never meets that refusal: this is what failed every sweep.
func TestTheSweepAsksOnlyWhatAPhoneMay(t *testing.T) {
	kiffc, calls := fakeKIFF(t, sweepAgents(), nil, false)
	if _, err := sweep(context.Background(), kiffc, []string{"taf_made"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range *calls {
		if strings.HasPrefix(c, "refused ") {
			t.Fatalf("the sweep made a call KIFF refuses a phone: %s", c)
		}
	}
	// And the fake does refuse what a phone may not: the test would see it.
	if a, err := kiffc.do(context.Background(), http.MethodGet, "/v1/keys", nil); err != nil || a.Status != http.StatusForbidden {
		t.Fatalf("the fake let a phone list keys: %v %v", a, err)
	}
}

// A taf the prune deleted between the sweep's read and its call is gone,
// not a failure.
func TestATafDeletedMeanwhileIsGone(t *testing.T) {
	kiffc, _ := fakeKIFF(t, sweepAgents(), []string{"taf_made"}, false)
	c, err := sweep(context.Background(), kiffc, []string{"taf_made"})
	if err != nil || c.gone != 1 || c.archived != 0 || c.failed != 0 {
		t.Fatalf("counts %+v, err %v", c, err)
	}
}

// A failed call is counted and returned, for the step to report, and the
// taf it didn't take is counted as left; the workflows keep the run's
// verdict (TestTheSweepNeverDecidesTheRun).
func TestAFailedSweepSaysSo(t *testing.T) {
	kiffc, _ := fakeKIFF(t, sweepAgents(), nil, true)
	c, err := sweep(context.Background(), kiffc, []string{"taf_made"})
	if err == nil || c.failed != 1 || c.left != 1 {
		t.Fatalf("counts %+v, err %v", c, err)
	}
}

// When KIFF doesn't answer the phone's access, the sweep says so and
// archives nothing: it never guesses which agents are there.
func TestTheSweepStopsWhenItCantReadTheAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden"}`))
	}))
	defer srv.Close()
	kiffc := &client{base: srv.URL, http: srv.Client(), auth: func(*http.Request, []byte) {}}
	c, err := sweep(context.Background(), kiffc, []string{"taf_made"})
	if err == nil || !strings.Contains(err.Error(), "reading the phone's access") || c != (sweepCounts{}) {
		t.Fatalf("counts %+v, err %v", c, err)
	}
}

// Archiving is a call KIFF takes from a phone's session only signed, and
// reading its access is not.
func TestTheSweepSignsItsArchive(t *testing.T) {
	if !signedPath(http.MethodPost, "/v1/me/agents/taf_made/archive") {
		t.Fatal("archiving an agent isn't signed")
	}
	if signedPath(http.MethodGet, "/v1/me/access") {
		t.Fatal("reading the phone's access is signed")
	}
}

// Each taf a run makes is added to -made, once, and a job with no run that
// got that far has none.
func TestARunRecordsItsTafsForTheSweep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "made.txt")
	for _, a := range []string{"taf_a", "taf_b", "taf_a"} {
		if err := recordMade(path, a); err != nil {
			t.Fatal(err)
		}
	}
	made, err := readMade(path)
	if err != nil || !slices.Equal(made, []string{"taf_a", "taf_b"}) {
		t.Fatalf("made %v, %v", made, err)
	}
	if made, err := readMade(filepath.Join(t.TempDir(), "none.txt")); err != nil || made != nil {
		t.Fatalf("no file: %v, %v", made, err)
	}
	if err := recordMade("", "taf_a"); err != nil {
		t.Fatal(err)
	}
}

// Both workflows on the test account sweep after every run, whatever the
// runs did, and a failed sweep never fails the run: the verdict is the
// steps before it.
func TestTheSweepNeverDecidesTheRun(t *testing.T) {
	for _, wf := range []string{"eval.yml", "e2e.yml"} {
		b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", wf))
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if !strings.Contains(s, "E2E_MADE: e2e-made.txt") {
			t.Fatalf("%s doesn't set E2E_MADE", wf)
		}
		i := strings.Index(s, "- name: The sweep")
		if i < 0 {
			t.Fatalf("%s has no sweep step", wf)
		}
		step := s[i:]
		if j := strings.Index(step[1:], "\n      - "); j >= 0 {
			step = step[:j+1]
		}
		for _, must := range []string{"(always())", "continue-on-error: true", "go run ./cmd/e2e -sweep", "E2E_KIFF_SESSION", "E2E_PHONE_KEY"} {
			if !strings.Contains(step, must) {
				t.Fatalf("%s's sweep step lacks %q:\n%s", wf, must, step)
			}
		}
		if strings.Contains(step, "ANTHROPIC_API_KEY") {
			t.Fatalf("%s's sweep step is given the model's key; it runs no model", wf)
		}
		// It comes after the last run of cmd/e2e.
		if last := strings.LastIndex(s, "go run ./cmd/e2e $("); last > i {
			t.Fatalf("%s runs cmd/e2e after the sweep", wf)
		}
	}
}
