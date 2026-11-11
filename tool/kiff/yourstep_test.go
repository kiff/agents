package kiff_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/kiff"
)

// fakeHandoffs is KIFF's /v1/me/handoffs: one hand-off per operation, whose
// status the test sets.
type fakeHandoffs struct {
	mu      sync.Mutex
	status  string
	got     []map[string]any
	code    int
	already bool
	// refusePrepared refuses a raise that names prepared_by, as KIFF does
	// when one of its calls isn't this agent's sent call.
	refusePrepared bool
}

func (f *fakeHandoffs) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != "/v1/me/handoffs" || r.Header.Get("Authorization") != "Bearer kiff_agent_secret" {
		http.Error(w, "no", http.StatusUnauthorized)
		return
	}
	b, _ := io.ReadAll(r.Body)
	var in map[string]any
	_ = json.Unmarshal(b, &in)
	f.got = append(f.got, in)
	if _, named := in["prepared_by"]; named && f.refusePrepared {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"prepared_by_invalid","detail":"prepared_by: op-x is not one of this agent's calls"}`))
		return
	}
	if f.code != 0 {
		w.WriteHeader(f.code)
		_, _ = w.Write([]byte(`{"error":"prepared_by_invalid","detail":"what is required, at most 300 characters"}`))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "ho_1", "status": f.status, "link_host": "app.example.com",
		"expires_at": "2026-11-18T17:00:00Z", "already_waiting": f.already})
}

func inRun(callID string) context.Context {
	return tool.WithCall(run.WithRef(context.Background(), run.Ref{Run: "run_1", Session: "s1"}), callID)
}

var stepInput = json.RawMessage(`{"what":"Approve 3 supplier payments","why":"Your bank asks you to approve transfers yourself","link":"https://app.example.com/r/1?t=x","about":"Batch 11 Nov"}`)

// Raised, the call pauses the run as a held call does, by the hand-off's id
// and the call's operation ID; collected after the answer, it is the
// owner's answer. Still waiting, collecting leaves the run paused.
func TestYourStepPausesAndCollectsTheAnswer(t *testing.T) {
	f := &fakeHandoffs{status: "waiting"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ys := kiff.NewYourStep(srv.URL, "kiff_agent_secret", nil)
	if ys.Spec().Name != kiff.YourStepName || ys.Gateway() || ys.Access() != tool.Own {
		t.Fatalf("spec %+v", ys.Spec())
	}
	res, err := ys.Call(inRun("call_1"), stepInput)
	if err != nil || !res.Held() || res.Hold == nil || res.Hold.Exception != "ho_1" || res.Hold.Operation == "" ||
		!res.Hold.Expires.Equal(time.Date(2026, 11, 18, 17, 0, 0, 0, time.UTC)) {
		t.Fatalf("raise = %+v, %v", res, err)
	}
	if op := run.OperationOf("run_1", "call_1", stepInput); res.Hold.Operation != op || f.got[0]["operation"] != op {
		t.Errorf("operation %q, sent %q, want %q", res.Hold.Operation, f.got[0]["operation"], op)
	}
	if f.got[0]["what"] != "Approve 3 supplier payments" || f.got[0]["link"] == "" || f.got[0]["about"] != "Batch 11 Nov" {
		t.Errorf("sent %+v", f.got[0])
	}
	q := run.Requirement{CallID: "call_1", Tool: kiff.YourStepName, Input: stepInput, Operation: res.Hold.Operation, Exception: "ho_1"}
	a := agent.Agent{Tools: []tool.Tool{ys}}
	if _, err := kiff.Collect(context.Background(), a, []run.Requirement{q}); !errors.Is(err, kiff.ErrNotFinal) {
		t.Fatalf("collected while waiting: %v", err)
	}
	for status, want := range map[string]string{"done": "says they did it", "skipped": "chose not to", "no_answer": "didn't answer"} {
		f.mu.Lock()
		f.status = status
		f.mu.Unlock()
		got, err := kiff.Collect(context.Background(), a, []run.Requirement{q})
		if err != nil || len(got) != 1 || !strings.Contains(got[0].Result.Text, want) || got[0].Result.Held() {
			t.Errorf("%s: %+v, %v", status, got, err)
		}
		if status == "no_answer" && !got[0].Result.IsError {
			t.Error("no answer reads as done")
		}
		if last := f.got[len(f.got)-1]; last["operation"] != q.Operation {
			t.Errorf("%s: collected with %q, want the call's operation", status, last["operation"])
		}
	}
}

// KIFF's refusal is the call's result, for the model: nothing was handed
// over, and the key never appears in an error.
func TestYourStepRefusedAndOutsideARun(t *testing.T) {
	f := &fakeHandoffs{code: http.StatusUnprocessableEntity}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ys := kiff.NewYourStep(srv.URL, "kiff_agent_secret", nil)
	res, err := ys.Call(inRun("call_1"), stepInput)
	if err != nil || !res.IsError || !strings.Contains(res.Text, "Nothing was handed") || res.Held() {
		t.Fatalf("refused = %+v, %v", res, err)
	}
	if res, err := ys.Call(context.Background(), stepInput); err != nil || !res.IsError {
		t.Errorf("outside a run = %+v, %v", res, err)
	}
	trial := run.WithTrial(inRun("call_2"), run.Trial{})
	if res, err := ys.Call(trial, stepInput); err != nil || res.Held() || len(f.got) != 1 {
		t.Errorf("a trial handed it over: %+v, %v, %d calls", res, err, len(f.got))
	}
	down := kiff.NewYourStep("http://127.0.0.1:1", "kiff_agent_secret", nil)
	if _, err := down.Call(inRun("call_3"), stepInput); err == nil || strings.Contains(err.Error(), "kiff_agent_secret") {
		t.Errorf("unreachable KIFF: %v", err)
	}
}

// A step an earlier run handed over and that still waits doesn't pause this
// run: it's told the step is already with the owner.
func TestYourStepAlreadyWaitingDoesntPause(t *testing.T) {
	f := &fakeHandoffs{status: "waiting", already: true}
	srv := httptest.NewServer(f)
	defer srv.Close()
	res, err := kiff.NewYourStep(srv.URL, "kiff_agent_secret", nil).Call(inRun("call_9"), stepInput)
	if err != nil || res.Held() || res.Hold != nil || res.IsError || !strings.Contains(res.Text, "already with your owner") {
		t.Fatalf("already waiting = %+v, %v", res, err)
	}
}

// The raise names what the run did to prepare the step: its calls that went
// through and that KIFF recorded, about the same case, newest first, by their
// operation IDs. A call KIFF didn't record, another case's, or one kept
// without its input is left out. Refused by KIFF, the step is raised again
// without them: it reaches its owner either way.
func TestYourStepNamesTheCallsThatPreparedIt(t *testing.T) {
	f := &fakeHandoffs{status: "waiting"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ys := kiff.NewYourStep(srv.URL, "kiff_agent_secret", nil)
	draft := json.RawMessage(`{"amount":4210,"kiff_about":"Batch 11 Nov"}`)
	named := json.RawMessage(`{"amount":1,"kiff_about":"Batch 11 Nov","kiff_operation_id":"draft-2"}`)
	done := []run.Done{
		{CallID: "c1", Tool: "bank_draft_transfer", Input: draft, Invocation: "inv_1"},
		{CallID: "c2", Tool: "bank_draft_transfer", Input: named, Invocation: "inv_2"},
		{CallID: "c3", Tool: "bank_draft_transfer", Input: json.RawMessage(`{"kiff_about":"Batch 4 Nov"}`), Invocation: "inv_3"},
		{CallID: "c4", Tool: "memory_write", Input: json.RawMessage(`{"kiff_about":"Batch 11 Nov"}`)},
		{CallID: "c5", Tool: "bank_draft_transfer", Invocation: "inv_5"},
	}
	ctx := run.WithDone(inRun("call_9"), done)
	if res, err := ys.Call(ctx, stepInput); err != nil || !res.Held() {
		t.Fatalf("raise = %+v, %v", res, err)
	}
	want := []any{run.OperationOf("run_1", "c2", named), run.OperationOf("run_1", "c1", draft)}
	if got, _ := f.got[0]["prepared_by"].([]any); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("prepared_by = %v, want %v", f.got[0]["prepared_by"], want)
	}
	// Without an about, every recorded call of the run, still not one KIFF didn't record.
	f.got = nil
	if _, err := ys.Call(ctx, json.RawMessage(`{"what":"Sign the lease","why":"Only you may sign"}`)); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.got[0]["prepared_by"].([]any); len(got) != 3 {
		t.Fatalf("prepared_by without about = %v, want c3, c2 and c1", f.got[0]["prepared_by"])
	}
	// Nothing done: no prepared_by at all.
	f.got = nil
	if _, err := ys.Call(inRun("call_8"), stepInput); err != nil {
		t.Fatal(err)
	}
	if _, sent := f.got[0]["prepared_by"]; sent {
		t.Fatalf("sent prepared_by with nothing done: %v", f.got[0])
	}
	// KIFF refuses the list: raised again without it, and still handed over.
	f.got, f.refusePrepared = nil, true
	res, err := ys.Call(ctx, stepInput)
	if err != nil || !res.Held() || len(f.got) != 2 {
		t.Fatalf("after a refused prepared_by: %+v, %v, %d raises", res, err, len(f.got))
	}
	if _, sent := f.got[1]["prepared_by"]; sent || f.got[1]["operation"] != f.got[0]["operation"] {
		t.Fatalf("the second raise %v: want the same operation, without prepared_by", f.got[1])
	}
}
