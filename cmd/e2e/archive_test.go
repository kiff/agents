package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/kiff/agents/cmd/internal/testaccount"
)

// A taf a run left unrecorded is archived as the owner's phone, by its first
// characters, and read back: done only once it has left the account's access.
// Setup's agent, an ambiguous prefix and a whole id send nothing, and the
// public log never names a whole id, even in KIFF's own words.
func TestArchiveTakesOneTafByItsPrefixAndReadsItBack(t *testing.T) {
	var mu sync.Mutex
	agents := []string{"agt_setup0000aaaa", "agt_qwBr3staleone", "agt_twin1aaaa", "agt_twin2bbbb", "agt_failsWholeId", "agt_stays00000", "refunds-i2pxkwpf", "refunds-ab12"}
	var archives []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/me/access":
			out := map[string]any{}
			for _, a := range agents {
				out[a] = map[string]any{"use": true, "manage": true}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"agents": out, "setup_agent": "agt_setup0000aaaa"})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/me/agents/") && strings.HasSuffix(r.URL.Path, "/archive"):
			a := strings.Split(r.URL.Path, "/")[4]
			archives = append(archives, a)
			switch a {
			case "agt_failsWholeId":
				// KIFF's words may name the whole id: they never reach the public log.
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"agent ` + a + ` can't be archived now"}`))
			case "agt_stays00000":
				// Answered as archived, yet still in the access read after.
				_, _ = w.Write([]byte(`{"status":"archived"}`))
			default:
				agents = slices.DeleteFunc(agents, func(x string) bool { return x == a })
				_, _ = w.Write([]byte(`{"status":"archived"}`))
			}
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()
	kiffc := &testaccount.Client{Base: srv.URL, HTTP: srv.Client(), Auth: func(*http.Request, []byte) {}}
	ctx := context.Background()

	var out strings.Builder
	if err := archiveAgent(ctx, kiffc, "agt_qwBr", &out); err != nil || len(archives) != 1 || archives[0] != "agt_qwBr3staleone" {
		t.Fatalf("the unrecorded taf, by its first characters: %v, %v\n%s", err, archives, out.String())
	}
	if !strings.Contains(out.String(), "taf agt_qwBr… archived") || strings.Contains(out.String(), "agt_qwBr3staleone") {
		t.Fatalf("the public log:\n%s", out.String())
	}
	for _, c := range []struct{ prefix, says string }{
		{"agt_setu", "Setup's agent"},                // the account's own, never a run's
		{"agt_twin", "2 of the test account's tafs"}, // ambiguous: nothing is sent
		{"agt_none", "0 of the test account's tafs"},
		{"agt_qwBr3staleone", "never its whole id"}, // a whole id given by mistake
		{"agt_", "never its whole id"},
		{"taf_qwBr", "0 of the test account's tafs"},
		{"refunds-ab12", "never its whole id"}, // an older id short enough to pass for a prefix
		{"refunds-", "2 of the test account's tafs"},
	} {
		n := len(archives)
		err := archiveAgent(ctx, kiffc, c.prefix, &out)
		if err == nil || !strings.Contains(err.Error(), c.says) || len(archives) != n {
			t.Fatalf("%s: %v, %d archives sent", c.prefix, err, len(archives)-n)
		}
	}
	// An older taf, whose id KIFF made from its job before agt_ ids.
	out.Reset()
	if err := archiveAgent(ctx, kiffc, "refunds-i2px", &out); err != nil || archives[len(archives)-1] != "refunds-i2pxkwpf" ||
		strings.Contains(out.String(), "refunds-i2pxkwpf") {
		t.Fatalf("an older taf by its first characters: %v %v\n%s", err, archives, out.String())
	}
	err := archiveAgent(ctx, kiffc, "agt_fail", &out)
	if err == nil || strings.Contains(err.Error(), "agt_failsWholeId") || !strings.Contains(err.Error(), "agt_fail…") {
		t.Fatalf("KIFF's refusal, in the public log: %v", err)
	}
	if err := archiveAgent(ctx, kiffc, "agt_stay", &out); err == nil || !strings.Contains(err.Error(), "still in the test account's access") {
		t.Fatalf("a taf still there after its archive was taken as done: %v", err)
	}
}

// Every step of the Eval that runs a model stops on its own limit, and those
// limits fit inside the job's: a stuck suite fails its step, and the sweep
// and the report after it still run.
func TestEachEvalSuiteStopsOnItsOwnLimit(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "eval.yml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	limit := func(block string) int {
		for _, l := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(l), "timeout-minutes:"); ok {
				n, err := strconv.Atoi(strings.TrimSpace(v))
				if err != nil {
					t.Fatalf("timeout-minutes %q", v)
				}
				return n
			}
		}
		return 0
	}
	steps := strings.Split(s, "\n      - ")
	job := limit(steps[0])
	sum, suites := 0, 0
	for _, step := range steps[1:] {
		if !strings.Contains(step, "ANTHROPIC_API_KEY") {
			continue
		}
		suites++
		name, _, _ := strings.Cut(step, "\n")
		n := limit(step)
		if n == 0 {
			t.Errorf("%s has no limit of its own", name)
		}
		sum += n
	}
	if suites < 7 {
		t.Fatalf("%d steps run a model, want the 7 suites at least", suites)
	}
	// Ten minutes for the checkout, the definitions, the sweep and the report.
	if job < sum+10 {
		t.Fatalf("the job's limit (%d) doesn't leave the sweep room after the suites' (%d in all)", job, sum)
	}
}
