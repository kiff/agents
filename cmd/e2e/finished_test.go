package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/kiff/agents/cmd/internal/testaccount"
)

// fakeFinished is planque's finished-grants route on one account: at most
// perCall a call, more: true while some are left, a held one skipped.
type fakeFinished struct {
	mu      sync.Mutex
	status  map[string]string
	held    map[string]bool
	perCall int
	calls   int
	// dropLive deletes a live grant too, as a planque bug would.
	dropLive bool
}

func (f *fakeFinished) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Planque-Device-Signature") == "" {
		http.Error(w, `{"error":"unsigned"}`, http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/accounts/tnt/grants":
		var gs []map[string]string
		for id, st := range f.status {
			if st == "disconnected" && r.URL.Query().Get("include") != "disconnected" {
				continue
			}
			gs = append(gs, map[string]string{"id": id, "status": st})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"grants": gs})
	case r.Method == http.MethodDelete && r.URL.Path == "/v1/accounts/tnt/finished-grants":
		f.calls++
		var drop []string
		skipped := 0
		for id, st := range f.status {
			if st != "disconnected" && st != "failed" {
				continue
			}
			if f.held[id] {
				skipped++
				continue
			}
			drop = append(drop, id)
		}
		sort.Strings(drop)
		more := len(drop) > f.perCall
		if more {
			drop = drop[:f.perCall]
		}
		for _, id := range drop {
			delete(f.status, id)
		}
		if f.dropLive {
			for id, st := range f.status {
				if st == "connected" {
					delete(f.status, id)
					break
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"grants": len(drop), "operations": 3 * len(drop), "skipped": skipped, "more": more})
	default:
		http.NotFound(w, r)
	}
}

func finishedAccount(finished, connected int) *fakeFinished {
	f := &fakeFinished{status: map[string]string{}, held: map[string]bool{}, perCall: 200}
	for i := 0; i < finished; i++ {
		st := "disconnected"
		if i%5 == 0 {
			st = "failed"
		}
		f.status[fmt.Sprintf("grt_fin%04dsecretid", i)] = st
	}
	for i := 0; i < connected; i++ {
		f.status[fmt.Sprintf("grt_live%04dsecretid", i)] = "connected"
	}
	return f
}

func finishedPhone(t *testing.T, f *fakeFinished) *testaccount.Phone {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return testaccount.NewPhone(key, srv.URL, "tnt", srv.Client())
}

// The finished grants go across as many calls as planque asks for, a held
// one stays, the live ones stay, and the public log says counts only.
func TestFinishedGrantsGoAcrossCallsAndTheLiveOnesStay(t *testing.T) {
	f := finishedAccount(960, 4)
	f.held["grt_fin0003secretid"] = true
	var out strings.Builder
	if err := deleteFinishedGrants(context.Background(), finishedPhone(t, f), &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if f.calls != 5 {
		t.Fatalf("%d calls for 959 grants at 200 a call\n%s", f.calls, out.String())
	}
	if len(f.status) != 5 || f.status["grt_fin0003secretid"] != "disconnected" {
		t.Fatalf("left: %v", f.status)
	}
	log := out.String()
	for _, must := range []string{"before: 964 grants (4 connected, 768 disconnected, 192 failed)", "call 5: 159 grants deleted",
		"after: 5 grants (4 connected, 1 disconnected)", "deleted: 959 grants, 2877 operations; 1 finished grants kept"} {
		if !strings.Contains(log, must) {
			t.Fatalf("the log lacks %q:\n%s", must, log)
		}
	}
	if strings.Contains(log, "secretid") {
		t.Fatalf("the public log names a grant:\n%s", log)
	}
}

// A live grant gone after the run fails it.
func TestFinishedGrantsFailIfALiveOneWent(t *testing.T) {
	f := finishedAccount(10, 2)
	f.dropLive = true
	var out strings.Builder
	err := deleteFinishedGrants(context.Background(), finishedPhone(t, f), &out)
	if err == nil || !strings.Contains(err.Error(), "the live grants changed: 2 before, 1 after") {
		t.Fatalf("%v\n%s", err, out.String())
	}
}

// planque answering more forever stops at the cap, as a failure.
func TestFinishedGrantsStopAtTheCap(t *testing.T) {
	f := finishedAccount(finishedCalls*3+1, 0)
	f.perCall = 3
	var out strings.Builder
	err := deleteFinishedGrants(context.Background(), finishedPhone(t, f), &out)
	if err == nil || f.calls != finishedCalls || !strings.Contains(err.Error(), "still has more") {
		t.Fatalf("%v after %d calls", err, f.calls)
	}
}

// The one-off runs by hand only, in the test account's group (never beside
// an Eval or E2E run), with the phone's session and key and nothing else.
func TestTheFinishedGrantsOneOffRunsAloneAsThePhone(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "e2e-finished-grants.yml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, must := range []string{"workflow_dispatch:", "group: test-account-2", "cancel-in-progress: false", "E2E_KIFF_SESSION", "E2E_PHONE_KEY",
		"go run ./cmd/e2e -finished-grants"} {
		if !strings.Contains(s, must) {
			t.Fatalf("e2e-finished-grants.yml lacks %q", must)
		}
	}
	for _, never := range []string{"schedule:", "ANTHROPIC_API_KEY", "inputs:"} {
		if strings.Contains(s, never) {
			t.Fatalf("e2e-finished-grants.yml has %q", never)
		}
	}
}
