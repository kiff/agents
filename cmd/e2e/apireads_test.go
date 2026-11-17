package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
)

// Eval 44162956815: the test tenant used planque's 100 API description reads
// for the day, and three suites spent 14 turns each on "can't connect", read
// as Coach's 0 of 3. The run that meets planque's cap stops the suite at once,
// as the harness's failure.
func TestASetupPlanqueRefusesForTheDaysAPIReadsStopsTheSuite(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	run := func(text string, isError bool, tool_ string) error {
		sessions := memory.New()
		if err := sessions.Create(ctx, session.Session{ID: "s1", Tenant: "t1", Agent: "setup", Started: now, Updated: now}); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(agent.ToolResultBody{CallID: "c1", Tool: tool_, Result: tool.Result{Text: text, IsError: isError}})
		if err := sessions.Begin(ctx, "t1", "s1", "r1", now); err != nil {
			t.Fatal(err)
		}
		if err := sessions.Append(ctx, "t1", "s1", "r1", []session.Event{{Session: "s1", Run: "r1", Seq: 1, Time: now, Kind: session.ToolResult, Body: b}}); err != nil {
			t.Fatal(err)
		}
		tt := &test{tenant: "t1", sessions: sessions, out: &redacting{w: io.Discard, ci: true}, cfg: config{log: io.Discard}}
		_, _, err := tt.latest(ctx, "s1", "r1")
		return err
	}
	capped := `planque answered 429: This account has read 100 API descriptions today. Try again tomorrow.`
	if err := run(capped, true, "planque"); !errors.Is(err, errAPIReadsCapped) {
		t.Fatalf("planque's cap: %v, want errAPIReadsCapped", err)
	}
	// Anything else goes on as before: another refusal, the same words from another tool, or not an error.
	for name, c := range map[string]struct {
		text    string
		isError bool
		tool    string
	}{
		"another planque refusal":   {"planque answered 403: This app can't be connected through tafs yet.", true, "planque"},
		"another tool's words":      {capped, true, "web_read"},
		"planque's words, no error": {capped, false, "planque"},
	} {
		if err := run(c.text, c.isError, c.tool); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
