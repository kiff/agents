package main

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
)

// A grant Setup asked for in a run the harness didn't start (r2, one Coach
// ran after an approval) is still this conversation's: read through r1, its
// request is kept, so "offers only its own connections" never calls it
// another test's grant.
func TestAGrantMadeInAnotherRunOfTheChatIsOurs(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	sessions := memory.New()
	if err := sessions.Create(ctx, session.Session{ID: "s1", Tenant: "t1", Agent: "setup", Started: now, Updated: now}); err != nil {
		t.Fatal(err)
	}
	call := func(run string, seq int64, id, input string) session.Event {
		b, _ := json.Marshal(agent.ToolCallBody{CallID: id, Tool: "planque", Input: json.RawMessage(input)})
		return session.Event{Session: "s1", Run: run, Seq: seq, Time: now, Kind: session.ToolCall, Body: b}
	}
	result := func(run string, seq int64, id, text string) session.Event {
		b, _ := json.Marshal(agent.ToolResultBody{CallID: id, Tool: "planque", Result: tool.Result{Text: text}})
		return session.Event{Session: "s1", Run: run, Seq: seq, Time: now, Kind: session.ToolResult, Body: b}
	}
	for _, r := range []struct {
		run    string
		events []session.Event
	}{
		{"r1", []session.Event{
			call("r1", 1, "c1", `{"action":"request","api":{"url":"https://sheet.test/openapi.json"},"name":"introduction-drafts","tools":[{"tool":"getRows"}]}`),
			result("r1", 2, "c1", `{"id":"g_sheet","declared":true}`),
		}},
		{"r2", []session.Event{
			call("r2", 1, "c2", `{"action":"request","api":{"url":"https://mail.test/openapi.json"},"name":"introduction-drafts","tools":[{"tool":"createDraft"}]}`),
			result("r2", 2, "c2", `{"id":"g_mail","declared":true}`),
		}},
	} {
		if err := sessions.Begin(ctx, "t1", "s1", r.run, now); err != nil {
			t.Fatal(err)
		}
		if err := sessions.Append(ctx, "t1", "s1", r.run, r.events); err != nil {
			t.Fatal(err)
		}
	}
	tt := &test{tenant: "t1", sessions: sessions, out: &redacting{w: io.Discard, ci: true}, cfg: config{log: io.Discard}}
	if _, _, err := tt.latest(ctx, "s1", "r1"); err != nil {
		t.Fatal(err)
	}
	approvals := []approvalSeen{{Grant: "g_sheet", Key: true}, {Grant: "g_mail", Key: true, Draft: "d1"}}
	for _, g := range gradeBatch(tt.asks, approvals, nil) {
		if g.Name == "offers only its own connections" && !g.Grade.Pass {
			t.Fatalf("a grant made in another run of this chat reads as another test's: %s", g.Grade.Why)
		}
	}
	// A grant no run of this chat asked for is still another test's.
	approvals = append(approvals, approvalSeen{Grant: "g_old", Key: true})
	for _, g := range gradeBatch(tt.asks, approvals, nil) {
		if g.Name == "offers only its own connections" && g.Grade.Pass {
			t.Fatal("a grant no run of this chat made passes as ours")
		}
	}
}
