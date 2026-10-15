package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kiff/agents/eval"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/session"
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
}

// passing is a judge model that passes everything, for 1M input tokens a turn.
type passing struct{}

func (passing) Turn(context.Context, model.Request, func(string)) (model.Reply, error) {
	return model.Reply{Blocks: []model.Block{{Kind: model.Text, Text: `{"pass": true}`}}, Usage: model.Usage{InputTokens: 1_000_000}}, nil
}

func TestRunWithAJudge(t *testing.T) {
	r := emptyRun("sesn_r")
	raw, _ := json.Marshal(eval.ToolCallBody{CallID: "1", Tool: "send_email", Server: "kiff", Input: json.RawMessage(`{"customer": "x"}`)})
	r.Events = append(r.Events, session.Event{Seq: 2, Kind: session.ToolCall, Body: raw})
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
