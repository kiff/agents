package kiff_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/kiff"
)

// answeredDone is KIFF's hand-offs API for a step its owner answered done: the
// raise names it done, and each check posted is kept, by hand-off.
type answeredDone struct {
	mu     sync.Mutex
	checks []string
}

func (f *answeredDone) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer kiff_agent_secret" {
		http.Error(w, "no", http.StatusUnauthorized)
		return
	}
	if id, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/v1/me/handoffs/"), "/check"); ok && r.Method == http.MethodPost {
		var in struct{ Operation string }
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &in)
		f.checks = append(f.checks, id+" "+in.Operation)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "status": "done"})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "ho_1", "status": "done", "expires_at": "2026-11-18T17:00:00Z"})
}

// A step its owner answered "done" is checked by the run's first read of the
// vendor about the same case, made after the answer: that read's operation ID
// goes to KIFF as the step's check, once. A write about the same case never
// counts, nor a read about another case, nor a second read, nor another run's
// read; and a step with no case is never checked by a read.
func TestTheRunsFirstReadAboutTheCaseChecksAStepAnsweredDone(t *testing.T) {
	api := &answeredDone{}
	srv := httptest.NewServer(api)
	defer srv.Close()
	gw, _ := gateway(t, false)
	tools, err := kiff.New(gw.URL, testKey, gw.Client()).Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	read, write := tools[0], tools[1]
	ys := kiff.NewYourStep(srv.URL, "kiff_agent_secret", nil)
	checks := &run.Checks{}
	base := run.WithChecks(run.WithRef(context.Background(), run.Ref{Run: "run_1", Session: "s1"}), checks)
	q := run.Requirement{CallID: "call_1", Tool: kiff.YourStepName, Input: stepInput, Operation: run.OperationOf("run_1", "call_1", stepInput), Exception: "ho_1"}
	if res, err := ys.Collect(base, q); err != nil || res.IsError {
		t.Fatalf("collect = %+v, %v", res, err)
	}
	call := func(tl tool.Tool, ctx context.Context, id, input string) {
		t.Helper()
		if _, err := tl.Call(tool.WithCall(ctx, id), json.RawMessage(input)); err != nil {
			t.Fatal(err)
		}
	}
	// A write about the same case, sent: never a check.
	call(write, base, "w1", `{"forwarded":true,"kiff_about":"Batch 11 Nov"}`)
	// A read about another case: not this step's check.
	call(read, base, "r0", `{"charge":"ch_1","kiff_about":"Batch 4 Nov"}`)
	// Another run's read about the same case: never this step's check.
	other := run.WithChecks(run.WithRef(context.Background(), run.Ref{Run: "run_2", Session: "s1"}), &run.Checks{})
	call(read, other, "r9", `{"charge":"ch_1","kiff_about":"Batch 11 Nov"}`)
	if len(api.checks) != 0 {
		t.Fatalf("checked by %v before the run's read about the case", api.checks)
	}
	readInput := `{"charge":"ch_1","kiff_about":"Batch 11 Nov"}`
	call(read, base, "r1", readInput)
	if want := "ho_1 " + run.OperationOf("run_1", "r1", json.RawMessage(readInput)); len(api.checks) != 1 || api.checks[0] != want {
		t.Fatalf("checks = %v, want [%s]", api.checks, want)
	}
	// A second read: the step has its check.
	call(read, base, "r2", readInput)
	if len(api.checks) != 1 {
		t.Fatalf("checked twice: %v", api.checks)
	}
	// A step with no case: no read checks it.
	none := &run.Checks{}
	ctx := run.WithChecks(run.WithRef(context.Background(), run.Ref{Run: "run_3", Session: "s1"}), none)
	bare := json.RawMessage(`{"what":"Sign the lease","why":"Only you may sign"}`)
	if _, err := ys.Collect(ctx, run.Requirement{CallID: "call_2", Tool: kiff.YourStepName, Input: bare, Operation: run.OperationOf("run_3", "call_2", bare), Exception: "ho_1"}); err != nil {
		t.Fatal(err)
	}
	call(read, ctx, "r3", `{"charge":"ch_1"}`)
	call(read, ctx, "r4", `{"charge":"ch_1","kiff_about":""}`)
	if len(api.checks) != 1 {
		t.Fatalf("a step with no case was checked: %v", api.checks)
	}
}
