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
	mu     sync.Mutex
	status string
	got    []map[string]string
	code   int
}

func (f *fakeHandoffs) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != "/v1/me/handoffs" || r.Header.Get("Authorization") != "Bearer kiff_agent_secret" {
		http.Error(w, "no", http.StatusUnauthorized)
		return
	}
	b, _ := io.ReadAll(r.Body)
	var in map[string]string
	_ = json.Unmarshal(b, &in)
	f.got = append(f.got, in)
	if f.code != 0 {
		w.WriteHeader(f.code)
		_, _ = w.Write([]byte(`{"error":"prepared_by_invalid","detail":"what is required, at most 300 characters"}`))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "ho_1", "status": f.status, "link_host": "app.example.com",
		"expires_at": "2026-11-18T17:00:00Z"})
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
