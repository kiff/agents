package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/eval"
	"github.com/kiff/agents/eval/cases"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/registry"
	runs "github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/kiff"
)

type fakeReader map[string]eval.Run

func (f fakeReader) Read(_ context.Context, id string) (eval.Run, error) { return f[id], nil }

func withReader(t *testing.T, f fakeReader) {
	t.Helper()
	old := newReader
	newReader = func(_, key string) reader {
		if key != "k" {
			t.Errorf("key %q", key)
		}
		return f
	}
	t.Cleanup(func() { newReader = old })
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func emptyRun(id string) eval.Run {
	raw, _ := json.Marshal(session.UsageBody{CostMicroUSD: 1_400_000})
	return eval.Run{Session: session.Session{ID: id}, Events: []session.Event{{Seq: 1, Kind: session.Usage, Body: raw}}}
}

func TestRunGradesAndComparesWithTheBaseline(t *testing.T) {
	withReader(t, fakeReader{"sesn_r": emptyRun("sesn_r")})
	dir := t.TempDir()
	out := filepath.Join(dir, "results.json")
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"-out", out, "refunds=sesn_r"}, &stdout, &stderr, env(map[string]string{"ANTHROPIC_API_KEY": "k"}))
	if err != nil {
		t.Fatal(err)
	}
	var results []eval.Result
	if err := json.Unmarshal(stdout.Bytes(), &results); err != nil || len(results) != 1 {
		t.Fatalf("stdout %s: %v", stdout.String(), err)
	}
	r := results[0]
	if r.Case != "refunds" || r.Session != "sesn_r" || r.CostMicroUSD != 1_400_000 || r.PerItemMicro != 200_000 || r.Passed != 2 || r.Graded != 6 {
		t.Errorf("result %+v", r)
	}
	if !strings.Contains(stderr.String(), "refunds  2/6  $1.4000 a run, $0.2000 an item, judge $0.0000") || !strings.Contains(stderr.String(), "FAIL  does the job: missed") {
		t.Errorf("stderr:\n%s", stderr.String())
	}
	if b, _ := os.ReadFile(out); !bytes.Equal(b, stdout.Bytes()) {
		t.Errorf("-out differs from stdout")
	}

	base := filepath.Join(dir, "base.json")
	_ = os.WriteFile(base, []byte(`[{"case": "refunds", "score": 0.9}]`), 0o644)
	err = run(context.Background(), []string{"-baseline", base, "refunds=sesn_r"}, &stdout, &stderr, env(map[string]string{"ANTHROPIC_API_KEY": "k"}))
	if err == nil || !strings.Contains(err.Error(), "lower than the baseline: refunds") {
		t.Fatalf("err %v; want a lower score to fail", err)
	}

	// A baseline from -repeat holds each case's mean, not its last run: 0.33
	// is above a mean of 0.30, and below one of 0.35 (#78).
	for scores, lower := range map[string]bool{"0.2, 0.4": false, "0.5, 0.2": true} {
		a, b, _ := strings.Cut(scores, ", ")
		_ = os.WriteFile(base, []byte(`[{"case": "refunds", "score": `+a+`}, {"case": "refunds", "score": `+b+`}]`), 0o644)
		err = run(context.Background(), []string{"-baseline", base, "refunds=sesn_r"}, io.Discard, io.Discard, env(map[string]string{"ANTHROPIC_API_KEY": "k"}))
		if got := err != nil && strings.Contains(err.Error(), "lower than the baseline: refunds"); got != lower {
			t.Errorf("a baseline of %s: %v", scores, err)
		}
	}
}

// passing is a judge model that passes everything, for 1M input tokens a turn.
type passing struct{}

func (passing) Turn(context.Context, model.Request, func(string)) (model.Reply, error) {
	return model.Reply{Blocks: []model.Block{{Kind: model.Text, Text: `{"pass": true}`}}, Usage: model.Usage{InputTokens: 1_000_000}}, nil
}

func TestRunWithAJudge(t *testing.T) {
	r := emptyRun("sesn_r")
	raw, _ := json.Marshal(eval.ToolCallBody{CallID: "1", Tool: "send_email", Server: "kiff", Input: json.RawMessage(`{"customer": "x"}`)})
	ok, _ := json.Marshal(eval.ToolResultBody{CallID: "1", Tool: "send_email", Text: "sent"})
	r.Events = append(r.Events, session.Event{Seq: 2, Kind: session.ToolCall, Body: raw}, session.Event{Seq: 3, Kind: session.ToolResult, Body: ok})
	withReader(t, fakeReader{"sesn_r": r})
	old := newModel
	newModel = func(key string) model.Model {
		if key != "k" {
			t.Errorf("judge key %q", key)
		}
		return passing{}
	}
	t.Cleanup(func() { newModel = old })

	var stdout bytes.Buffer
	args := []string{"-judge-model", "judge", "-judge-price", "5,25", "refunds=sesn_r"}
	if err := run(context.Background(), args, &stdout, &bytes.Buffer{}, env(map[string]string{"ANTHROPIC_API_KEY": "k"})); err != nil {
		t.Fatal(err)
	}
	var results []eval.Result
	if err := json.Unmarshal(stdout.Bytes(), &results); err != nil {
		t.Fatal(err)
	}
	if c := results[0].Checks[4]; !c.Pass || c.Skipped || results[0].JudgeMicroUSD != 5 {
		t.Fatalf("disclosure %+v, judge cost %d", c, results[0].JudgeMicroUSD)
	}
}

func TestRunRefusesBadArguments(t *testing.T) {
	withReader(t, fakeReader{})
	for _, tt := range []struct {
		args []string
		env  map[string]string
		want string
	}{
		{nil, map[string]string{"ANTHROPIC_API_KEY": "k"}, "at least one"},
		{[]string{"refunds=s"}, nil, "ANTHROPIC_API_KEY"},
		{[]string{"billpay=s"}, map[string]string{"ANTHROPIC_API_KEY": "k"}, "want case=session"},
		{[]string{"refunds"}, map[string]string{"ANTHROPIC_API_KEY": "k"}, "want case=session"},
		{[]string{"-judge-model", "j", "refunds=s"}, map[string]string{"ANTHROPIC_API_KEY": "k"}, "-judge-price"},
	} {
		err := run(context.Background(), tt.args, &bytes.Buffer{}, &bytes.Buffer{}, env(tt.env))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: err %v; want %q", tt.args, err, tt.want)
		}
	}
}

// script is an agent model that answers each turn with the next reply, and
// keeps what it was asked.
type script struct {
	replies []model.Reply
	asked   []model.Request
}

func (s *script) Turn(_ context.Context, req model.Request, _ func(string)) (model.Reply, error) {
	s.asked = append(s.asked, req)
	next := s.replies[0]
	s.replies = s.replies[1:]
	return next, nil
}

// refundTool is a gateway tool through KIFF that refunds.
type refundTool struct{}

func (refundTool) Spec() model.ToolSpec {
	return model.ToolSpec{Name: "create_refund", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (refundTool) Access() tool.Access { return tool.Write }
func (refundTool) Gateway() bool       { return true }
func (refundTool) Call(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{Text: `{"id":"re_1","status":"succeeded"}`}, nil
}

func withAgent(t *testing.T, m *script) {
	t.Helper()
	oldModel, oldConnect := newModel, connect
	newModel = func(key string) model.Model {
		if key != "k" {
			t.Errorf("model key %q", key)
		}
		return m
	}
	connect = func(_ context.Context, gateway, key string) ([]tool.Tool, error) {
		if key != "kiff-k" || gateway != "https://gateway.test/mcp" {
			t.Errorf("connected to %s with %q", gateway, key)
		}
		return []tool.Tool{refundTool{}}, nil
	}
	// Every Card has room unless a test says otherwise.
	oldRoom := cardRoom
	cardRoom = func(context.Context, string, string) ([]kiff.Room, error) { return plenty(), nil }
	t.Cleanup(func() { newModel, connect, cardRoom = oldModel, oldConnect, oldRoom })
}

// plenty is room for any case, on every tool the cases use.
func plenty() []kiff.Room {
	var out []kiff.Room
	for _, s := range []cases.Scenario{cases.Refunds, cases.Chargebacks} {
		for action := range s.Room {
			out = append(out, kiff.Room{Card: "c-" + action, Action: action, Quantity: "count", Window: "calendar_day", Limit: 1 << 40, Remaining: 1 << 40})
		}
	}
	return out
}

func jobsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "refunds.md"), []byte("You handle refund requests.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func agentTurns() []model.Reply {
	use := model.Reply{Blocks: []model.Block{{Kind: model.ToolUse, CallID: "c1", Tool: "create_refund", Input: json.RawMessage(`{"charge":"ch_1"}`)}},
		Stop: model.CallTools, Usage: model.Usage{InputTokens: 1_000_000}}
	done := model.Reply{Blocks: []model.Block{{Kind: model.Text, Text: "Refunded one order."}}, Stop: model.EndTurn}
	return []model.Reply{use, done}
}

// With -run, each case runs on this runtime first: an eval run of its job's
// agent through KIFF's gateway, with tool calls kept, then graded.
func TestRunACaseOnThisRuntime(t *testing.T) {
	m := &script{replies: agentTurns()}
	withAgent(t, m)
	skill := filepath.Join(t.TempDir(), "skill.md")
	_ = os.WriteFile(skill, []byte("Every write goes through KIFF."), 0o644)
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"-run", "-model", "claude-opus-5-5", "-price", "5000000,25000000", "-jobs", jobsDir(t),
		"-skill", skill, "-gateway", "https://gateway.test/mcp", "refunds"}, &stdout, &stderr,
		env(map[string]string{"ANTHROPIC_API_KEY": "k", "KIFF_AGENT_KEY": "kiff-k"}))
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	var results []eval.Result
	if err := json.Unmarshal(stdout.Bytes(), &results); err != nil || len(results) != 1 {
		t.Fatalf("stdout %s: %v", stdout.String(), err)
	}
	if r := results[0]; r.Case != "refunds" || r.Session == "" || r.CostMicroUSD != 5_000_000 || r.Graded == 0 {
		t.Errorf("result %+v", r)
	}
	first := m.asked[0]
	if first.Model != "claude-opus-5-5" || !strings.Contains(first.System, "You handle refund requests.") ||
		!strings.Contains(first.System, "Every write goes through KIFF.") {
		t.Errorf("the agent was asked with model %q, system %q", first.Model, first.System)
	}
	if got := first.Messages[0].Blocks[0].Text; got != cases.Refunds.Input {
		t.Errorf("the run's input %q, want the case's", got)
	}
}

// The run keeps its tool calls and results for grading, and is an eval run.
func TestARuntimeRunKeepsWhatTheChecksRead(t *testing.T) {
	m := &script{replies: agentTurns()}
	withAgent(t, m)
	rt := newRuntime(m, "kiff-k", registry.Definition{Tenant: evalTenant, Model: "m", Price: model.Price{Input: 1, Output: 1},
		MaxTokens: 1024, Gateway: "https://gateway.test/mcp", KeyRef: "KIFF_AGENT_KEY", Keep: session.DefaultPolicy(evalKeep), // Run keeps tool calls whatever this says
		MaxCost: 2_000_000, History: 20}, jobsDir(t), "", io.Discard)
	r, err := rt.Run(context.Background(), cases.Refunds)
	if err != nil {
		t.Fatal(err)
	}
	calls, err := r.Calls()
	if err != nil || r.Status != runs.Completed || len(calls) != 1 || calls[0].Tool != "create_refund" {
		t.Fatalf("status %s, calls %+v, %v", r.Status, calls, err)
	}
	list, _ := rt.runs.List(context.Background(), evalTenant, "refunds", 1)
	if len(list) != 1 || list[0].Trigger != runs.ByEval {
		t.Fatalf("runs %+v", list)
	}
}

func TestRunNeeds(t *testing.T) {
	withAgent(t, &script{})
	jobs := jobsDir(t)
	full := map[string]string{"ANTHROPIC_API_KEY": "k", "KIFF_AGENT_KEY": "kiff-k"}
	for name, c := range map[string]struct {
		args []string
		env  map[string]string
		want string
	}{
		"no KIFF key":   {[]string{"-run", "-model", "m", "-price", "1,1", "-jobs", jobs, "refunds"}, map[string]string{"ANTHROPIC_API_KEY": "k"}, "KIFF_AGENT_KEY"},
		"no model":      {[]string{"-run", "-price", "1,1", "-jobs", jobs, "refunds"}, full, "-model"},
		"no jobs":       {[]string{"-run", "-model", "m", "-price", "1,1", "refunds"}, full, "-jobs"},
		"no price":      {[]string{"-run", "-model", "m", "-jobs", jobs, "refunds"}, full, "-price"},
		"no case":       {[]string{"-run", "-model", "m", "-price", "1,1", "-jobs", jobs}, full, "name at least one case"},
		"a session":     {[]string{"-run", "-model", "m", "-price", "1,1", "-jobs", jobs, "refunds=sesn_1"}, full, "want a case"},
		"no job's file": {[]string{"-run", "-model", "m", "-price", "1,1", "-jobs", jobs, "chargebacks"}, full, "the job's instructions"},
	} {
		err := run(context.Background(), c.args, io.Discard, io.Discard, env(c.env))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want it to mention %q", name, err, c.want)
		}
	}
}

// grade runs the refunds case on the runtime with the agent's turns, and
// returns each check's verdict by name.
func grade(t *testing.T, turns []model.Reply) map[string]eval.CheckResult {
	t.Helper()
	m := &script{replies: turns}
	withAgent(t, m)
	rt := newRuntime(m, "kiff-k", registry.Definition{Tenant: evalTenant, Model: "m", Price: model.Price{Input: 1, Output: 1},
		MaxTokens: 1024, Gateway: "https://gateway.test/mcp", KeyRef: "KIFF_AGENT_KEY", MaxCost: 2_000_000, History: 20},
		jobsDir(t), "", io.Discard)
	r, err := rt.Run(context.Background(), cases.Refunds)
	if err != nil {
		t.Fatal(err)
	}
	res, err := cases.Refunds.Case().Grade(context.Background(), r, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]eval.CheckResult{}
	for _, c := range res.Checks {
		out[c.Name] = c
	}
	return out
}

func calls(blocks ...model.Block) model.Reply {
	return model.Reply{Blocks: blocks, Stop: model.CallTools}
}

func call(id, name, input string) model.Block {
	return model.Block{Kind: model.ToolUse, CallID: id, Tool: name, Input: json.RawMessage(input)}
}

// The cases grade this runtime's calls as they grade the baseline's: a call
// through KIFF's gateway is KIFF's, the memory tool is the agent's own, and
// a write the injected instruction asked for is still caught.
func TestTheChecksGradeThisRuntimesCalls(t *testing.T) {
	end := model.Reply{Blocks: []model.Block{{Kind: model.Text, Text: "Done."}}, Stop: model.EndTurn}
	got := grade(t, []model.Reply{
		calls(call("c1", "memory", `{"action":"list"}`), call("c2", "create_refund", `{"charge":"ch_1","metadata":{"order":"T-1001"}}`)),
		end,
	})
	if c := got["writes only through KIFF"]; !c.Pass {
		t.Errorf("a gateway call and the memory tool: %+v", c)
	}
	if c := got["ignores injected instructions"]; !c.Pass {
		t.Errorf("no injected write: %+v", c)
	}

	got = grade(t, []model.Reply{calls(call("c1", "create_refund", `{"metadata":{"order":"`+cases.Refunds.Injected+`"}}`)), end})
	if c := got["ignores injected instructions"]; c.Pass {
		t.Errorf("an injected write through the gateway passed: %+v", c)
	}
}

// interrupting is a model whose first turn interrupts the command, as Ctrl+C
// would, and asks for a tool call.
type interrupting struct {
	stop  context.CancelFunc
	turns int
}

func (m *interrupting) Turn(context.Context, model.Request, func(string)) (model.Reply, error) {
	m.turns++
	m.stop()
	time.Sleep(50 * time.Millisecond) // the interrupt lands while the turn runs
	return calls(call("c1", "create_refund", `{"charge":"ch_1"}`)), nil
}

// An interrupt stops the run at its next checkpoint: no model turn after it.
func TestAnInterruptStopsTheRun(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	m := &interrupting{stop: stop}
	withAgent(t, &script{})
	rt := newRuntime(m, "kiff-k", registry.Definition{Tenant: evalTenant, Model: "m", Price: model.Price{Input: 1, Output: 1},
		MaxTokens: 1024, Gateway: "https://gateway.test/mcp", KeyRef: "KIFF_AGENT_KEY", MaxCost: 2_000_000, History: 20},
		jobsDir(t), "", io.Discard)
	r, err := rt.Run(ctx, cases.Refunds)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != runs.Cancelled || m.turns != 1 {
		t.Fatalf("after an interrupt: %s after %d model turns; want CANCELLED after 1", r.Status, m.turns)
	}
}

// An interrupt during one case starts no later case: the command prints
// what it graded and fails, naming the cases it didn't start.
func TestAnInterruptStartsNoLaterCase(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	m := &interrupting{stop: stop}
	withAgent(t, &script{})
	newModel = func(string) model.Model { return m }
	jobs := jobsDir(t)
	_ = os.WriteFile(filepath.Join(jobs, "chargebacks.md"), []byte("You answer disputes."), 0o644)
	var stdout bytes.Buffer
	err := run(ctx, []string{"-run", "-model", "m", "-price", "1,1", "-jobs", jobs, "-gateway", "https://gateway.test/mcp", "refunds", "chargebacks"},
		&stdout, io.Discard, env(map[string]string{"ANTHROPIC_API_KEY": "k", "KIFF_AGENT_KEY": "kiff-k"}))
	if err == nil || !strings.Contains(err.Error(), "not started: chargebacks") {
		t.Fatalf("err %v, want the interrupt to name chargebacks", err)
	}
	if m.turns != 1 {
		t.Fatalf("%d model turns after the interrupt, want 1", m.turns)
	}
	var results []eval.Result
	if err := json.Unmarshal(stdout.Bytes(), &results); err != nil || len(results) != 1 || results[0].Case != "refunds" {
		t.Fatalf("printed %s: want refunds' result", stdout.String())
	}
	rt := newRuntime(m, "kiff-k", registry.Definition{Tenant: evalTenant}, jobs, "", io.Discard)
	if _, err := rt.Run(ctx, cases.Chargebacks); err == nil || !strings.Contains(err.Error(), "not started") {
		t.Fatalf("Run after an interrupt: %v", err)
	}
}

// With -notes, the case's note is in the agent's memory before the run: the
// agent reads it with its memory tool. A case with no note file starts with
// none.
func TestTheOwnersNoteIsInMemory(t *testing.T) {
	m := &script{replies: []model.Reply{
		calls(call("c1", "memory", `{"action":"list"}`)),
		{Blocks: []model.Block{{Kind: model.Text, Text: "Done."}}, Stop: model.EndTurn},
	}}
	withAgent(t, m)
	notes := t.TempDir()
	_ = os.WriteFile(filepath.Join(notes, "refunds.md"), []byte("Offer store credit to Ada first.\n"), 0o644)
	rt := newRuntime(m, "kiff-k", registry.Definition{Tenant: evalTenant, Model: "m", Price: model.Price{Input: 1, Output: 1},
		MaxTokens: 1024, Gateway: "https://gateway.test/mcp", KeyRef: "KIFF_AGENT_KEY", MaxCost: 2_000_000, History: 20},
		jobsDir(t), "", io.Discard)
	rt.notes = notes
	if _, err := rt.Run(context.Background(), cases.Refunds); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(m.asked[1].Messages)
	if !strings.Contains(string(b), "Offer store credit to Ada first.") {
		t.Fatalf("the agent's memory didn't hold the note: %s", b)
	}
	if got, _ := rt.sessions.ListNotes(context.Background(), evalTenant, "chargebacks"); len(got) != 0 {
		t.Fatalf("a case with no note file has notes: %+v", got)
	}
}

// A price is input,output, or input,output,cache-read,cache-write: with cache
// prices, the run's turns ask for the cache (#77).
func TestAPriceMayCarryCachePrices(t *testing.T) {
	for s, want := range map[string]model.Price{
		"5,25":       {Input: 5, Output: 25},
		"4,20,1,5":   {Input: 4, Output: 20, CacheRead: 1, CacheWrite: 5},
		" 4, 20,1,5": {Input: 4, Output: 20, CacheRead: 1, CacheWrite: 5},
	} {
		if p, err := parsePrice(s); err != nil || p != want {
			t.Errorf("%q: %+v, %v", s, p, err)
		}
	}
	for _, s := range []string{"", "5", "5,25,1", "5,25,0,5", "5,25,1,-5", "5,25,1,5,1", "a,b"} {
		if _, err := parsePrice(s); err == nil {
			t.Errorf("%q was accepted", s)
		}
	}
}

// heldTool is a write KIFF holds for the owner.
type heldTool struct{ refundTool }

func (heldTool) Call(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{Text: "Held for the owner.", State: tool.StateHeld,
		Hold: &tool.Hold{Operation: "op_1", Exception: "ex_1", Expires: time.Now().Add(time.Hour)}}, nil
}

// A run KIFF holds a call of is graded where it paused, and the hold is
// withdrawn with the agent's key, so it never waits on the owner or carries
// into the next run (#78).
func TestARunsHeldCallsAreWithdrawn(t *testing.T) {
	turns := []model.Reply{agentTurns()[0], {Blocks: []model.Block{{Kind: model.Text, Text: "Waiting on the owner."}}, Stop: model.EndTurn}}
	m := &script{replies: slices.Clone(turns)}
	withAgent(t, m)
	connect = func(context.Context, string, string) ([]tool.Tool, error) { return []tool.Tool{heldTool{}}, nil }
	var asked []string
	old := withdraw
	withdraw = func(_ context.Context, api, key, exception string) (kiff.Withdrawal, error) {
		asked = append(asked, api+" "+key+" "+exception)
		return kiff.Withdrawal{Withdrawn: true, Status: "invalidated"}, nil
	}
	t.Cleanup(func() { withdraw = old })
	var log bytes.Buffer
	rt := newRuntime(m, "kiff-k", registry.Definition{Tenant: evalTenant, Model: "m", Price: model.Price{Input: 1, Output: 1},
		MaxTokens: 1024, Gateway: "https://gateway.test/mcp", KeyRef: "KIFF_AGENT_KEY", MaxCost: 2_000_000, History: 20}, jobsDir(t), "", &log)
	r, err := rt.Run(context.Background(), cases.Refunds)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != runs.Paused {
		t.Fatalf("status %s", r.Status)
	}
	if len(asked) != 1 || asked[0] != kiff.APIURL+" kiff-k ex_1" {
		t.Fatalf("withdrew %v", asked)
	}
	if !strings.Contains(log.String(), "withdrew KIFF's hold on create_refund (ex_1)") {
		t.Fatalf("log %q", log.String())
	}

	// A hold that can't be withdrawn is logged for the owner; nothing fails.
	log.Reset()
	m.replies = slices.Clone(turns)
	withdraw = func(context.Context, string, string, string) (kiff.Withdrawal, error) {
		return kiff.Withdrawal{}, errors.New("KIFF answered 500")
	}
	if _, err := rt.Run(context.Background(), cases.Refunds); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "couldn't withdraw create_refund (ex_1): KIFF answered 500: refuse it in KIFF") {
		t.Fatalf("log %q", log.String())
	}
}

// With -definitions, a case runs as its job's agent as kiff/tafs defines
// it: model, price, effort, max tokens, max turns, history and
// instructions, with the eval's own gateway, key and cost cap (#78). A field
// this runtime doesn't know fails the run, as the server would refuse it.
func TestACaseRunsAsTafsDefinesIt(t *testing.T) {
	// Turns that cost next to nothing, so the cost cap doesn't stop the run first.
	m := &script{replies: []model.Reply{calls(call("c1", "create_refund", `{"charge":"ch_1"}`)), agentTurns()[1]}}
	withAgent(t, m)
	defs := t.TempDir()
	def := `{"instructions": "You handle refund requests. The kiff skill: …", "model": "claude-opus-5-5",
		"price": {"Input": 4000000, "Output": 20000000, "CacheRead": 200000, "CacheWrite": 5000000},
		"max_tokens": 16000, "effort": "high", "gateway": "https://mcp.kiff.dev/mcp", "key_ref": "kiff/taf-1",
		"keep": {}, "max_cost_micro_usd": 200000000, "max_turns": 1, "history": 10}`
	_ = os.WriteFile(filepath.Join(defs, "refunds.json"), []byte(def), 0o644)
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"-run", "-definitions", defs, "-gateway", "https://gateway.test/mcp", "-max-cost", "1", "refunds"},
		&stdout, &stderr, env(map[string]string{"ANTHROPIC_API_KEY": "k", "KIFF_AGENT_KEY": "kiff-k"}))
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	first := m.asked[0]
	if first.Model != "claude-opus-5-5" || first.Effort != "high" || first.MaxTokens != 16000 || !first.Cache ||
		first.System != "You handle refund requests. The kiff skill: …" {
		t.Fatalf("asked %+v", first)
	}
	if len(m.asked) != 1 { // the definition's one turn: the server would stop it there too
		t.Fatalf("%d model turns with max_turns 1", len(m.asked))
	}

	_ = os.WriteFile(filepath.Join(defs, "refunds.json"), []byte(`{"model": "m", "temperature": 1}`), 0o644)
	m = &script{replies: agentTurns()}
	withAgent(t, m)
	err = run(context.Background(), []string{"-run", "-definitions", defs, "-gateway", "https://gateway.test/mcp", "refunds"},
		io.Discard, io.Discard, env(map[string]string{"ANTHROPIC_API_KEY": "k", "KIFF_AGENT_KEY": "kiff-k"}))
	if err == nil || !strings.Contains(err.Error(), `unknown field "temperature"`) {
		t.Fatalf("an unknown field: %v", err)
	}
	// A workspace the eval can't give is refused, not dropped (#86).
	_ = os.WriteFile(filepath.Join(defs, "refunds.json"), []byte(`{"model": "m", "workspace": true}`), 0o644)
	err = run(context.Background(), []string{"-run", "-definitions", defs, "-gateway", "https://gateway.test/mcp", "refunds"},
		io.Discard, io.Discard, env(map[string]string{"ANTHROPIC_API_KEY": "k", "KIFF_AGENT_KEY": "kiff-k"}))
	if err == nil || !strings.Contains(err.Error(), "has a workspace") {
		t.Fatalf("a workspace: %v", err)
	}
	err = run(context.Background(), []string{"-run", "-definitions", defs, "-model", "m", "refunds"},
		io.Discard, io.Discard, env(map[string]string{"ANTHROPIC_API_KEY": "k", "KIFF_AGENT_KEY": "kiff-k"}))
	if err == nil || !strings.Contains(err.Error(), "one or the other") {
		t.Fatalf("-definitions with -model: %v", err)
	}
}

// fakeStripe answers just enough for a Chargebacks seed, and counts seeds.
type fakeStripe struct {
	mu    sync.Mutex
	n     int
	seeds int
}

func (f *fakeStripe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = r.ParseForm()
	f.n++
	switch {
	case r.Method == "GET" && r.URL.Path == "/v1/disputes" && r.Form.Get("payment_intent") != "":
		fmt.Fprintf(w, `{"data": [{"id": "dp_%d"}], "has_more": false}`, f.n)
	case r.Method == "GET" && r.URL.Path == "/v1/disputes":
		f.seeds++ // each seed starts by closing what the last one left
		fmt.Fprint(w, `{"data": [], "has_more": false}`)
	default:
		fmt.Fprintf(w, `{"id": "x_%d", "latest_charge": "ch_%d"}`, f.n, f.n)
	}
}

// -seed seeds the case before each of -repeat runs, with the case's owner
// note in memory; the report and the baseline hold the case over its runs,
// against its mean score (#78).
func TestRepeatedSeededRunsAreReportedOverTheirMean(t *testing.T) {
	done := model.Reply{Blocks: []model.Block{{Kind: model.Text, Text: "Done."}}, Stop: model.EndTurn}
	replies := []model.Reply{ // the first run writes a note the second must not see
		calls(call("c1", "memory", `{"action":"list"}`), call("c2", "memory", `{"action":"write","name":"left","text":"dp_from_run_1"}`)), done,
		calls(call("c1", "memory", `{"action":"list"}`)), done,
	}
	m := &script{replies: replies}
	withAgent(t, m)
	f := &fakeStripe{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	old := stripeURL
	stripeURL = srv.URL
	t.Cleanup(func() { stripeURL = old })

	dir := t.TempDir()
	defs := filepath.Join(dir, "defs")
	_ = os.Mkdir(defs, 0o755)
	_ = os.WriteFile(filepath.Join(defs, "chargebacks.json"), []byte(`{"instructions": "You work the disputes.", "model": "claude-opus-5-5",
		"price": {"Input": 4000000, "Output": 20000000, "CacheRead": 200000, "CacheWrite": 5000000}, "max_tokens": 16000, "effort": "high", "history": 10}`), 0o644)
	_ = os.WriteFile(filepath.Join(defs, "source.json"), []byte(`{"repo": "kiff/tafs", "commit": "1d1ed42e64e88083f9fceb52c51eec81db1adbd8"}`), 0o644)
	base := filepath.Join(dir, "baseline.json")
	_ = os.WriteFile(base, []byte(`[{"case": "chargebacks", "score": 1}]`), 0o644)
	report := filepath.Join(dir, "report.md")

	var stdout bytes.Buffer
	err := run(context.Background(), []string{"-run", "-definitions", defs, "-gateway", "https://gateway.test/mcp",
		"-seed", "-repeat", "2", "-report", report, "-baseline", base, "chargebacks"}, &stdout, io.Discard,
		env(map[string]string{"ANTHROPIC_API_KEY": "k", "KIFF_AGENT_KEY": "kiff-k", "STRIPE_API_KEY": "sk_test_x"}))
	if err == nil || !strings.Contains(err.Error(), "lower than the baseline: chargebacks") {
		t.Fatalf("against a baseline of 1: %v", err)
	}
	var results []eval.Result
	if err := json.Unmarshal(stdout.Bytes(), &results); err != nil || len(results) != 2 {
		t.Fatalf("results %s: %v", stdout.String(), err)
	}
	if f.seeds != 2 {
		t.Fatalf("seeded %d times for 2 runs", f.seeds)
	}
	for i := range 2 {
		b, _ := json.Marshal(m.asked[2*i+1].Messages)
		if !strings.Contains(string(b), "Gus Regular") {
			t.Fatalf("run %d: the seed's owner note isn't in memory: %s", i+1, b)
		}
	}
	if b, _ := json.Marshal(m.asked[3].Messages); strings.Contains(string(b), "dp_from_run_1") {
		t.Fatalf("run 2 saw what run 1 wrote in memory: %s", b)
	}
	md, _ := os.ReadFile(report)
	for _, want := range []string{"**Lower than the baseline:** chargebacks", "## Configuration: chargebacks",
		"| Definition | kiff/tafs@1d1ed42e64e8 |", "| Effort | high |", "| Price | 4000000,20000000,200000,5000000 |",
		"| Tools | create_refund |", "| chargebacks | 2 |", "### chargebacks, run 2"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("no %q in the report:\n%s", want, md)
		}
	}

	// -seed and -ids together, -seed without a Stripe key, or -seed for a
	// case with no seeder, are refused before anything runs.
	for name, c := range map[string]struct {
		args []string
		env  map[string]string
	}{
		"-ids too":      {[]string{"-run", "-definitions", defs, "-seed", "-ids", base, "chargebacks"}, map[string]string{"ANTHROPIC_API_KEY": "k", "KIFF_AGENT_KEY": "kiff-k", "STRIPE_API_KEY": "sk_test_x"}},
		"no Stripe key": {[]string{"-run", "-definitions", defs, "-seed", "chargebacks"}, map[string]string{"ANTHROPIC_API_KEY": "k", "KIFF_AGENT_KEY": "kiff-k"}},
		"-repeat 0":     {[]string{"-run", "-definitions", defs, "-repeat", "0", "chargebacks"}, map[string]string{"ANTHROPIC_API_KEY": "k", "KIFF_AGENT_KEY": "kiff-k"}},
		"no seeder":     {[]string{"-run", "-definitions", defs, "-seed", "chargebacks", "refunds"}, map[string]string{"ANTHROPIC_API_KEY": "k", "KIFF_AGENT_KEY": "kiff-k", "STRIPE_API_KEY": "sk_test_x"}},
	} {
		m.asked, f.n, f.seeds = nil, 0, 0
		if err := run(context.Background(), c.args, io.Discard, io.Discard, env(c.env)); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if len(m.asked) != 0 || f.n != 0 {
			t.Errorf("%s: ran %d model turns and %d Stripe requests first", name, len(m.asked), f.n)
		}
	}
}

// A public log (-public, in CI) shows each run's score, cost and failed
// checks' names, and nothing that names the run: no session, no check's
// reason, no hold's exception, no results on stdout. -out still holds them
// all, and an error keeps its words with its ids hidden (#78).
func TestAPublicLogNamesNothing(t *testing.T) {
	withReader(t, fakeReader{"sesn_r": emptyRun("sesn_r")})
	out := filepath.Join(t.TempDir(), "results.json")
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"-public", "-out", out, "refunds=sesn_r"}, &stdout, &stderr, env(map[string]string{"ANTHROPIC_API_KEY": "k"})); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout %q, want nothing", stdout.String())
	}
	log := stderr.String()
	if !strings.Contains(log, "refunds  2/6  $1.4000 a run, $0.2000 an item, judge $0.0000  (…)") || !strings.Contains(log, "FAIL  does the job\n") {
		t.Errorf("log:\n%s", log)
	}
	if strings.Contains(log, "sesn_r") || strings.Contains(log, "missed") {
		t.Errorf("the public log names the run or a reason:\n%s", log)
	}
	if b, _ := os.ReadFile(out); !bytes.Contains(b, []byte(`"sesn_r"`)) {
		t.Errorf("-out lost the details: %s", b)
	}

	// An error's ids are hidden, its words kept.
	if got := redactIDs("seed: dispute dp_1Qx2Y3z4 on charge ch_3URYjrHcvoaK not found (exc_01HABCDEF)"); got != "seed: dispute … on charge … not found (…)" {
		t.Errorf("redacted %q", got)
	}

	// A hold's exception isn't named either.
	turns := []model.Reply{agentTurns()[0], {Blocks: []model.Block{{Kind: model.Text, Text: "Waiting on the owner."}}, Stop: model.EndTurn}}
	m := &script{replies: slices.Clone(turns)}
	withAgent(t, m)
	connect = func(context.Context, string, string) ([]tool.Tool, error) { return []tool.Tool{heldTool{}}, nil }
	old := withdraw
	withdraw = func(context.Context, string, string, string) (kiff.Withdrawal, error) {
		return kiff.Withdrawal{Withdrawn: true, Status: "invalidated"}, nil
	}
	t.Cleanup(func() { withdraw = old })
	var rlog bytes.Buffer
	rt := newRuntime(m, "kiff-k", registry.Definition{Tenant: evalTenant, Model: "m", Price: model.Price{Input: 1, Output: 1},
		MaxTokens: 1024, Gateway: "https://gateway.test/mcp", KeyRef: "KIFF_AGENT_KEY", MaxCost: 2_000_000, History: 20}, jobsDir(t), "", &rlog)
	rt.public = true
	if _, err := rt.Run(context.Background(), cases.Refunds); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rlog.String(), "withdrew KIFF's hold on create_refund (…)") || strings.Contains(rlog.String(), "ex_1") {
		t.Fatalf("log %q", rlog.String())
	}
}

// A run its Cards can't fund isn't run or graded (#78): with less room than
// the case needs on any limit of a tool it uses, or no Card for one, -run
// stops before seeding, and says which tool, what's left and when it resets.
func TestARunTheCardsCantFundIsNotGraded(t *testing.T) {
	ctx := context.Background()
	rt := newRuntime(&script{}, "kiff-k", registry.Definition{Tenant: evalTenant, Gateway: "https://gateway.test/mcp"}, "", "", io.Discard)
	old := cardRoom
	t.Cleanup(func() { cardRoom = old })
	reset := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	var asked []string
	room := func(update int64) func(context.Context, string, string) ([]kiff.Room, error) {
		return func(_ context.Context, gateway, key string) ([]kiff.Room, error) {
			asked = append(asked, gateway+" "+key)
			rooms := plenty()
			for i := range rooms {
				if rooms[i].Action == "update_dispute" {
					rooms[i] = kiff.Room{Card: "chargebacks-eval-update-dispute", Action: "update_dispute", Quantity: "sum(amount)",
						Window: "calendar_day", Limit: 1000000, Remaining: update, ResetsAt: reset}
				}
			}
			return rooms, nil
		}
	}

	cardRoom = room(1000)
	err := rt.checkRoom(ctx, cases.Chargebacks)
	if !errors.Is(err, errNotGraded) || !strings.Contains(err.Error(), "update_dispute has 1000 of 1000000 left on its Card (sum(amount) calendar_day, resets at 2026-10-19T00:00:00Z); a run needs 58500") {
		t.Fatalf("err %v", err)
	}
	if len(asked) != 1 || asked[0] != "https://gateway.test/mcp kiff-k" {
		t.Fatalf("asked %v", asked)
	}

	cardRoom = room(58500)
	if err := rt.checkRoom(ctx, cases.Chargebacks); err != nil {
		t.Fatalf("exactly enough: %v", err)
	}

	cardRoom = func(context.Context, string, string) ([]kiff.Room, error) { return nil, nil }
	if err := rt.checkRoom(ctx, cases.Chargebacks); !errors.Is(err, errNotGraded) || !strings.Contains(err.Error(), "no Card of the agent's covers") {
		t.Fatalf("no Cards: %v", err)
	}

	// A case that says nothing about room isn't checked.
	cardRoom = func(context.Context, string, string) ([]kiff.Room, error) { t.Fatal("read the room"); return nil, nil }
	if err := rt.checkRoom(ctx, cases.Scenario{Name: "plain"}); err != nil {
		t.Fatal(err)
	}
}
