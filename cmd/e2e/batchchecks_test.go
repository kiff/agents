package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/eval"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool"
)

func callEvent(t *testing.T, id string, input any) session.Event {
	t.Helper()
	in, _ := json.Marshal(input)
	b, _ := json.Marshal(agent.ToolCallBody{CallID: id, Tool: "planque", Input: in})
	return session.Event{Kind: session.ToolCall, Body: b}
}

func resultEvent(t *testing.T, id, text string, isErr bool) session.Event {
	t.Helper()
	b, _ := json.Marshal(agent.ToolResultBody{CallID: id, Tool: "planque", Result: tool.Result{Text: text, IsError: isErr}})
	return session.Event{Kind: session.ToolResult, Body: b}
}

func byName(rs []eval.CheckResult) map[string]eval.Grade {
	out := map[string]eval.Grade{}
	for _, r := range rs {
		out[r.Name] = r.Grade
	}
	return out
}

// Setup's planque requests are read from its session, each with what planque
// answered; other actions and other tools aren't requests.
func TestSetupsConnectionRequestsAreReadWithTheirAnswers(t *testing.T) {
	events := []session.Event{
		callEvent(t, "c1", map[string]any{"action": "request", "entry": "qonto-key@2", "name": "overdue", "uses": []string{"list_client_invoices"}}),
		resultEvent(t, "c1", "planque answered 400: the body must be a JSON object with the documented fields: unknown field \"tools\"\nmore", true),
		callEvent(t, "c2", map[string]any{"action": "status", "grant": "grt_1"}),
		resultEvent(t, "c2", `{"id":"grt_1"}`, false),
		callEvent(t, "c3", map[string]any{"action": "request", "entry": "qonto-key@2", "name": "overdue"}),
		resultEvent(t, "c3", `{"id":"grt_2","status":"pending","declared":true}`, false),
	}
	asks := connectAsksOf(events)
	if len(asks) != 2 {
		t.Fatalf("asks: %+v", asks)
	}
	if asks[0].Refused == "" || strings.Contains(asks[0].Refused, "more") || len(asks[0].Uses) != 1 {
		t.Fatalf("the refused one: %+v", asks[0])
	}
	if asks[1].Refused != "" || asks[1].Grant != "grt_2" {
		t.Fatalf("the one that went through: %+v", asks[1])
	}
}

// The batch's four gates, each failing for what it grades and skipped when
// the run didn't do it.
func TestTheCoachBatchsGates(t *testing.T) {
	ok := []connectAsk{{Entry: "qonto-key@2", Uses: []string{"list_client_invoices"}, Grant: "grt_2"},
		{Entry: "connector:composio:gmail", Uses: []string{"GMAIL_SEND_EMAIL"}, Grant: "grt_3"}}
	g := byName(gradeBatch(ok, []approvalSeen{{Grant: "grt_2", Draft: "drf_1", Key: true}, {Grant: "grt_3"}}, nil))
	for _, name := range []string{"connects on the first try", "offers only its own connections", "one approval for the key and the Card", "narrows a managed sign-in"} {
		if !g[name].Pass {
			t.Errorf("%s: %+v", name, g[name])
		}
	}

	// A refused request fails the first try, naming planque's words.
	refused := append([]connectAsk{{Entry: "qonto-key@2", Refused: "unknown field \"uses\""}}, ok...)
	if r := byName(gradeBatch(refused, nil, nil))["connects on the first try"]; r.Pass || !strings.Contains(r.Why, "unknown field") {
		t.Errorf("a refused request: %+v", r)
	}
	// Another taf's or a test's grant, reused or put to the owner.
	if r := byName(gradeBatch(append(ok, connectAsk{Reuse: "grt_old", Grant: "grt_4"}), nil, nil))["offers only its own connections"]; r.Pass || !strings.Contains(r.Why, "grt_old") {
		t.Errorf("a reused grant this run didn't make: %+v", r)
	}
	// A sign-in of a taf this run made, reused for the next: the account's own.
	if r := byName(gradeBatch(append(ok, connectAsk{Reuse: "grt_ours", Grant: "grt_6"}), nil, map[string]bool{"grt_ours": true}))["offers only its own connections"]; !r.Pass {
		t.Errorf("a reused grant of this run's taf: %+v", r)
	}
	if r := byName(gradeBatch(ok, []approvalSeen{{Grant: "grt_other"}}, nil))["offers only its own connections"]; r.Pass {
		t.Errorf("an approval for another grant: %+v", r)
	}
	// An approval put to the owner with no request at all: failed, not skipped.
	if r := byName(gradeBatch(nil, []approvalSeen{{Grant: "grt_other"}}, nil))["offers only its own connections"]; r.Pass || r.Skipped {
		t.Errorf("an approval for a grant the run never asked for: %+v", r)
	}
	// A key asked for before its Card: two approvals where one would do.
	two := []approvalSeen{{Grant: "grt_2", Key: true}, {Grant: "grt_2", Draft: "drf_1", Key: true}}
	if r := byName(gradeBatch(ok, two, nil))["one approval for the key and the Card"]; r.Pass || !strings.Contains(r.Why, "grt_2") {
		t.Errorf("the key before its Card: %+v", r)
	}
	// A managed app asked with no uses.
	wide := []connectAsk{{Entry: "connector:composio:gmail", Grant: "grt_3"}}
	if r := byName(gradeBatch(wide, nil, nil))["narrows a managed sign-in"]; r.Pass || r.Skipped {
		t.Errorf("a managed app with no uses: %+v", r)
	}

	// A key connected in a case that ends at "connected" (desk, billing): no
	// Card approved anywhere, so there's nothing to sign with the key.
	connectOnly := byName(gradeBatch([]connectAsk{{Entry: "", API: true, Grant: "grt_7"}}, []approvalSeen{{Grant: "grt_7", Key: true}, {Grant: "grt_7", Key: true}}, nil))
	if r := connectOnly["one approval for the key and the Card"]; !r.Skipped || r.Pass {
		t.Errorf("a key with no Card in the case: %+v", r)
	}

	// Nothing of it in the run: each skipped, never passed.
	for name, r := range byName(gradeBatch(nil, nil, nil)) {
		if !r.Skipped || r.Pass {
			t.Errorf("%s with nothing to grade: %+v", name, r)
		}
	}
	// No key grant, no managed app: those two skipped, the others graded.
	g = byName(gradeBatch([]connectAsk{{Entry: "stripe", Grant: "grt_5"}}, []approvalSeen{{Grant: "grt_5"}}, nil))
	if !g["one approval for the key and the Card"].Skipped || !g["narrows a managed sign-in"].Skipped || !g["connects on the first try"].Pass {
		t.Errorf("a sign-in alone: %+v", g)
	}
}

// Setup's requests are kept as each run is read: a chat drops a run's tool
// calls once it ends (session.UntilEnd), so the session read after the chat
// has none (run 43351837182: "no connection was asked for").
func TestRequestsAreKeptRunByRun(t *testing.T) {
	tt := &test{}
	// Run 1: the request, its answer not read yet.
	tt.keepAsks(connectAsksOf([]session.Event{callEvent(t, "c1", map[string]any{"action": "request", "server": "https://pay.example/mcp", "name": "refunds"})}))
	// Read again with its answer.
	tt.keepAsks(connectAsksOf([]session.Event{
		callEvent(t, "c1", map[string]any{"action": "request", "server": "https://pay.example/mcp", "name": "refunds"}),
		resultEvent(t, "c1", `{"id":"grt_1","status":"pending"}`, false),
	}))
	// Run 2: its calls are gone; a request of its own.
	tt.keepAsks(connectAsksOf([]session.Event{
		callEvent(t, "c2", map[string]any{"action": "request", "reuse": "grt_1", "name": "refunds"}),
		resultEvent(t, "c2", "planque answered 409: taken", true),
	}))
	// A later read with nothing in it changes nothing.
	tt.keepAsks(connectAsksOf(nil))
	if len(tt.asks) != 2 || tt.asks[0].Grant != "grt_1" || tt.asks[1].Refused == "" {
		t.Fatalf("%+v", tt.asks)
	}
	g := byName(gradeBatch(tt.asks, []approvalSeen{{Grant: "grt_1"}}, nil))
	if !g["offers only its own connections"].Pass {
		t.Errorf("the grant this chat made, its request read in an earlier run: %+v", g["offers only its own connections"])
	}
}
