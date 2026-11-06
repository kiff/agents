package runner_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool"
)

// A run's start is recorded on its first turn step by step, in order, and
// logged once with times only: no id, no input (the latency
// hunt). The steps add up to the start.
func TestARunsStartIsRecordedStepByStep(t *testing.T) {
	ctx := context.Background()
	rn := newRunner(&replies{answer("Refunded order 42.")}, tool.Result{Text: "refunded re_1"})
	var logged bytes.Buffer
	rn.Log = slog.New(slog.NewJSONHandler(&logged, nil))
	r, err := rn.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "Refund order 42."})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rn.Execute(ctx, "t1", r.ID, nil); err != nil {
		t.Fatal(err)
	}
	_, events, err := rn.Sessions.Read(ctx, "t1", r.Session)
	if err != nil {
		t.Fatal(err)
	}
	var u session.UsageBody
	for _, e := range events {
		if e.Kind == session.Usage {
			if err := json.Unmarshal(e.Body, &u); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if u.Timing == nil {
		t.Fatal("the first turn has no timing")
	}
	var names []string
	var sum int64
	for _, sp := range u.Timing.Start {
		names = append(names, sp.Name)
		sum += sp.MS
	}
	want := []string{"before", "run", "claim", "switching", "halted", "access", "definition", "key", "connect", "tools and skills", "build", "session", "history", "recording", "preface"}
	if !slices.Equal(names, want) {
		t.Fatalf("the start's steps %q, want %q", names, want)
	}
	if sum > u.Timing.StartMS+1 {
		t.Fatalf("the steps add up to %d ms, the start took %d", sum, u.Timing.StartMS)
	}
	line := logged.String()
	if strings.Count(line, "\n") != 1 || !strings.Contains(line, `"msg":"run started"`) || !strings.Contains(line, `"connect_ms":`) ||
		!strings.Contains(line, `"tools_and_skills_ms":`) || !strings.Contains(line, `"start_ms":`) {
		t.Fatalf("logged %q", line)
	}
	for _, private := range []string{r.ID, r.Session, "Refund order", `"t1"`, "rae"} {
		if strings.Contains(line, private) {
			t.Fatalf("the log line names %q: %s", private, line)
		}
	}
}
