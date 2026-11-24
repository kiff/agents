package kiff_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/kiff"
)

// pauseRig is a gateway whose browse_read is answered paused in the owner's
// Chrome (run "run_1", grant "grt_shop") until pausedLeft runs out, and a
// planque that answers the grant's run state from states, one per read.
type pauseRig struct {
	mu         sync.Mutex
	ops        []string
	pausedLeft int
	states     []string
	reads      int
	keys       []string
	gw, pq     *httptest.Server
}

func newPauseRig(t *testing.T, paused int, states ...string) *pauseRig {
	t.Helper()
	r := &pauseRig{pausedLeft: paused, states: states}
	r.gw = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var msg struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(req.Body).Decode(&msg)
		if msg.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any = map[string]any{"capabilities": map[string]any{}}
		switch msg.Method {
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "browse_read", "inputSchema": map[string]any{"type": "object"}, "annotations": map[string]any{"readOnlyHint": true}}}}
		case "tools/call":
			var p struct {
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			op, _ := p.Arguments["kiff_operation_id"].(string)
			r.mu.Lock()
			r.ops = append(r.ops, op)
			paused := r.pausedLeft > 0
			r.pausedLeft--
			r.mu.Unlock()
			if paused {
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "Refused by planque: the owner paused this run in their Chrome."}}, "isError": true,
					"structuredContent": map[string]any{"refused_by": "connection", "code": "paused_by_owner", "sent": false, "run": "run_1", "grant": "grt_shop"},
					"_meta":             map[string]any{"dev.kiff/call": map[string]any{"state": "forwarded", "sent": "yes"}}}
			} else {
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"title":"Lamp"}`}},
					"_meta": map[string]any{"dev.kiff/call": map[string]any{"state": "forwarded", "sent": "yes"}}}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
	}))
	r.pq = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.keys = append(r.keys, req.Header.Get("Authorization"))
		if req.URL.Path != "/v1/accounts/tnt_1/grants/grt_shop/chrome/run" {
			http.NotFound(w, req)
			return
		}
		state := "paused"
		if r.reads < len(r.states) {
			state = r.states[r.reads]
		}
		r.reads++
		_ = json.NewEncoder(w).Encode(map[string]string{"run": "run_1", "state": state})
	}))
	t.Cleanup(func() { r.gw.Close(); r.pq.Close() })
	return r
}

func (r *pauseRig) call(t *testing.T, planque string, input string) tool.Result {
	t.Helper()
	tools, err := kiff.New(r.gw.URL, testKey, r.gw.Client()).WithPlanque(planque, "tnt_1").Tools(context.Background())
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools: %v %v", tools, err)
	}
	ctx := tool.WithCall(run.WithRef(context.Background(), run.Ref{Session: "s1", Run: "run-a"}), "toolu_1")
	out, err := tools[0].Call(ctx, json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func fastPause(t *testing.T) {
	every, wait := kiff.PauseEvery, kiff.PauseWait
	kiff.PauseEvery, kiff.PauseWait = 5*time.Millisecond, 2*time.Second
	t.Cleanup(func() { kiff.PauseEvery, kiff.PauseWait = every, wait })
}

// A step the person paused in their Chrome waits for them: the run's state is
// read at planque with the taf's own key, never through the gateway, and on
// Resume the step is sent once more as a new operation, whose answer is the call's.
func TestAPausedStepWaitsForResumeAndIsSentOnceMore(t *testing.T) {
	fastPause(t)
	r := newPauseRig(t, 1, "paused", "paused", "working")
	out := r.call(t, r.pq.URL, `{"url":"https://shop.example/","kiff_operation_id":"mine"}`)
	if out.IsError || out.Text == "" {
		t.Fatalf("after Resume: %+v", out)
	}
	if len(r.ops) != 2 || r.ops[0] == r.ops[1] || r.ops[1] == "" {
		t.Fatalf("operations sent: %v", r.ops)
	}
	if r.reads != 3 {
		t.Fatalf("planque read %d times, want 3", r.reads)
	}
	for _, k := range r.keys {
		if k != "Bearer "+testKey {
			t.Fatalf("planque was asked with %q", k)
		}
	}
}

// Paused again after Resume: it waits again, sending once per Resume, never in a loop.
func TestAStepPausedAgainAfterResumeWaitsAgain(t *testing.T) {
	fastPause(t)
	r := newPauseRig(t, 2, "working", "paused", "working")
	out := r.call(t, r.pq.URL, `{"url":"https://shop.example/"}`)
	if out.IsError || len(r.ops) != 3 {
		t.Fatalf("after two Resumes: %+v, ops %v", out, r.ops)
	}
}

// Stopped, a run that's no longer the grant's, planque not answering, or no
// planque configured: the pause goes to the model as it came, at once.
func TestAPausedStepThatWontResumeGoesToTheModel(t *testing.T) {
	fastPause(t)
	for name, states := range map[string][]string{"stopped": {"paused", "stopped"}, "ended": {"ended"}} {
		r := newPauseRig(t, 1, states...)
		if out := r.call(t, r.pq.URL, `{"url":"https://shop.example/"}`); !out.IsError || len(r.ops) != 1 {
			t.Errorf("%s: %+v, ops %v", name, out, r.ops)
		}
	}
	down := newPauseRig(t, 1)
	down.pq.Close()
	if out := down.call(t, down.pq.URL, `{"url":"https://shop.example/"}`); !out.IsError || len(down.ops) != 1 {
		t.Errorf("planque down: %+v", out)
	}
	none := newPauseRig(t, 1, "working")
	if out := none.call(t, "", `{"url":"https://shop.example/"}`); !out.IsError || none.reads != 0 {
		t.Errorf("no planque configured: %+v, reads %d", out, none.reads)
	}
}

// A pause that never ends: the call waits PauseWait at most, then the model gets it.
func TestAPauseThatLastsGoesToTheModelAfterTheWait(t *testing.T) {
	every, wait := kiff.PauseEvery, kiff.PauseWait
	kiff.PauseEvery, kiff.PauseWait = 5*time.Millisecond, 60*time.Millisecond
	t.Cleanup(func() { kiff.PauseEvery, kiff.PauseWait = every, wait })
	r := newPauseRig(t, 1)
	start := time.Now()
	if out := r.call(t, r.pq.URL, `{"url":"https://shop.example/"}`); !out.IsError || len(r.ops) != 1 {
		t.Fatalf("a lasting pause: %+v", out)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("waited %s", time.Since(start))
	}
}

// Paused again while planque still says working (the Chrome's paused state late or lost): no re-send on every tick,
// only after a paused read, so a run that won't go on makes one more call per Resume, not one per tick.
func TestAStepPausedAgainIsSentOncePerResumeNotOncePerTick(t *testing.T) {
	fastPause(t)
	r := newPauseRig(t, 100, "working", "working", "working", "working", "working", "paused", "working", "ended")
	if out := r.call(t, r.pq.URL, `{"url":"https://shop.example/"}`); !out.IsError {
		t.Fatalf("still paused: %+v", out)
	}
	// The first call, one re-send on the first working, one more after the paused read; none on the other workings.
	if len(r.ops) != 3 {
		t.Fatalf("calls sent: %d (%v)", len(r.ops), r.ops)
	}
}
