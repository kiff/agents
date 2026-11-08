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
	"time"
)

var sweepNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) *time.Time { t := sweepNow.Add(-d); return &t }

const day = 24 * time.Hour

// The test account's keys: the account's, a connector's, Setup's, a gateway
// agent's, and tafs' draft keys, old and recent, this job's and not.
func sweepKeys() []listedKey {
	old := sweepNow.Add(-30 * day)
	return []listedKey{
		{ID: "k_account", Label: "Eval", CreatedAt: old, LastUsedAt: ago(20 * day)},
		{ID: "k_connector", Label: "planque", Roles: []string{"connector"}, CreatedAt: old, LastUsedAt: ago(20 * day)},
		{ID: "k_setup", Label: "draft e2e setup", Roles: []string{"setup_reader"}, AgentID: "e2e-setup", CreatedAt: old, LastUsedAt: ago(20 * day)},
		{ID: "k_gateway", Label: "chargebacks", Roles: []string{gatewayAgent}, AgentID: "gw", CreatedAt: old, LastUsedAt: ago(20 * day)},
		{ID: "k_old", Label: "draft Refunds", Roles: []string{gatewayAgent}, AgentID: "taf_old", CreatedAt: old, LastUsedAt: ago(15 * day)},
		{ID: "k_unused", Label: "draft Reembolsos", Roles: []string{gatewayAgent}, AgentID: "taf_unused", CreatedAt: old},
		{ID: "k_recent", Label: "draft Refunds", Roles: []string{gatewayAgent}, AgentID: "taf_recent", CreatedAt: old, LastUsedAt: ago(2 * day)},
		{ID: "k_new", Label: "draft Refunds", Roles: []string{gatewayAgent}, AgentID: "taf_new", CreatedAt: sweepNow.Add(-3 * day)},
		{ID: "k_made", Label: "draft Refunds", Roles: []string{gatewayAgent}, AgentID: "taf_made", CreatedAt: sweepNow.Add(-time.Hour), LastUsedAt: ago(time.Minute)},
		// One taf, two keys: one still in use keeps both.
		{ID: "k_mixed_old", Label: "draft Refunds", Roles: []string{gatewayAgent}, AgentID: "taf_mixed", CreatedAt: old, LastUsedAt: ago(20 * day)},
		{ID: "k_mixed_new", Label: "draft Refunds", Roles: []string{gatewayAgent}, AgentID: "taf_mixed", CreatedAt: old, LastUsedAt: ago(day)},
		// A taf's agent that also holds another kind of key isn't a taf here.
		{ID: "k_odd_draft", Label: "draft Refunds", Roles: []string{gatewayAgent}, AgentID: "taf_odd", CreatedAt: old, LastUsedAt: ago(20 * day)},
		{ID: "k_odd_other", Label: "by hand", Roles: []string{gatewayAgent}, AgentID: "taf_odd", CreatedAt: old, LastUsedAt: ago(20 * day)},
	}
}

// The sweep takes this job's tafs whenever they were used, and other tafs
// only once all their keys are idle for 14 days. Never Setup, a gateway or
// connector agent, or the account's key, even when -made names them.
func TestTheSweepTakesThisRunsTafsAndOldOnesOnly(t *testing.T) {
	agents, keysOf := sweepPlan(sweepKeys(), []string{"taf_made", "gw", "e2e-setup"}, sweepNow)
	want := []string{"taf_old", "taf_unused", "taf_made"}
	if !slices.Equal(agents, want) {
		t.Fatalf("swept %v, want %v", agents, want)
	}
	for _, a := range want {
		if len(keysOf[a]) != 1 {
			t.Fatalf("%s's keys = %v", a, keysOf[a])
		}
	}
	// Without -made, this job's taf waits its 14 days like any other.
	if agents, _ := sweepPlan(sweepKeys(), nil, sweepNow); slices.Contains(agents, "taf_made") {
		t.Fatalf("an unrecorded recent taf was swept: %v", agents)
	}
}

// fakeKIFF is the test account's keys in KIFF: archiving a known agent
// revokes its keys (as KIFF does since agents are archived with their keys);
// an agent the prune deleted is a 404, and its keys are revoked one by one.
// It records each call that changes something.
func fakeKIFF(t *testing.T, keys []listedKey, gone []string, failArchive bool) (*client, *[]string) {
	t.Helper()
	var changed []string
	revoke := func(match func(listedKey) bool) {
		keys = slices.DeleteFunc(keys, match)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/keys":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/me/agents/") && strings.HasSuffix(r.URL.Path, "/archive"):
			changed = append(changed, "archive "+strings.Split(r.URL.Path, "/")[4])
			a := strings.Split(r.URL.Path, "/")[4]
			switch {
			case failArchive:
				w.WriteHeader(http.StatusInternalServerError)
			case slices.Contains(gone, a):
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"no agent record named ` + a + `"}`))
			default:
				revoke(func(k listedKey) bool { return k.AgentID == a })
				_, _ = w.Write([]byte(`{"status":"archived"}`))
			}
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/keys/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/keys/")
			changed = append(changed, "revoke "+id)
			revoke(func(k listedKey) bool { return k.ID == id })
			w.WriteHeader(http.StatusNoContent)
		default:
			changed = append(changed, "unexpected "+r.Method+" "+r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &client{base: srv.URL, http: srv.Client(), auth: func(*http.Request, []byte) {}}, &changed
}

// A taf whose agent is there is archived, which takes its keys; one the
// prune already deleted has its key revoked. Nothing else is touched.
func TestTheSweepArchivesOrRevokesAndNothingElse(t *testing.T) {
	kiffc, changed := fakeKIFF(t, sweepKeys(), []string{"taf_unused"}, false)
	c, err := sweep(context.Background(), kiffc, []string{"taf_made"}, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"archive taf_old", "archive taf_unused", "revoke k_unused", "archive taf_made"}
	if !slices.Equal(*changed, want) {
		t.Fatalf("calls %v, want %v", *changed, want)
	}
	if c != (sweepCounts{archived: 2, revoked: 1, left: 0}) {
		t.Fatalf("counts %+v", c)
	}
	if s := c.String(); strings.Contains(s, "taf_") || strings.Contains(s, "k_") {
		t.Fatalf("the public log names an id: %s", s)
	}
}

// A failed call is counted and returned, for the step to report; the
// workflows keep the run's verdict (TestTheSweepNeverDecidesTheRun).
func TestAFailedSweepSaysSo(t *testing.T) {
	kiffc, _ := fakeKIFF(t, sweepKeys(), nil, true)
	c, err := sweep(context.Background(), kiffc, nil, sweepNow)
	if err == nil || c.failed != 2 || c.left != 2 {
		t.Fatalf("counts %+v, err %v", c, err)
	}
}

// Revoking a key is a call KIFF takes from a phone's session only signed.
func TestTheSweepSignsAKeysRevocation(t *testing.T) {
	if !signedPath(http.MethodDelete, "/v1/keys/k1") {
		t.Fatal("DELETE /v1/keys/{id} isn't signed")
	}
	if signedPath(http.MethodGet, "/v1/keys") {
		t.Fatal("listing keys is signed")
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
