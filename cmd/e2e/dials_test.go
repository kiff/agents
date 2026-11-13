package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool"
)

func tafsCall(t *testing.T, id, name string, input any) session.Event {
	t.Helper()
	in, _ := json.Marshal(input)
	b, _ := json.Marshal(agent.ToolCallBody{CallID: id, Tool: name, Server: agent.GatewayServer, Input: in})
	return session.Event{Kind: session.ToolCall, Body: b}
}

func tafsResult(t *testing.T, id, name, text string, isErr, notMade bool) session.Event {
	t.Helper()
	b, _ := json.Marshal(agent.ToolResultBody{CallID: id, Tool: name, Result: tool.Result{Text: text, IsError: isErr}, NotMade: notMade})
	return session.Event{Kind: session.ToolResult, Body: b}
}

func knobCard(t *testing.T, id, taf string, version int) session.Event {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"type": "knob", "id": id, "taf_id": taf, "version": version})
	return session.Event{Kind: session.Component, Body: b}
}

// The Sheets setup on a phone (13 Nov): the Card, then the schedule's own
// fingerprint (a knob card), its check failing, a version 2 drafted and its
// install held for the owner. The bar counted none of it; now the dial's
// switches are approvals apart from the Card, and each version drafted
// during setup is a loop.
func TestADialApartFromTheCard(t *testing.T) {
	events := []session.Event{
		tafsCall(t, "c1", "taf_dial", map[string]any{"taf": "Follow-up leads", "schedule": "mon 09:00"}),
		tafsResult(t, "c1", "taf_dial", `{"taf":"Follow-up leads","taf_id":"taf_1","version":2}`, false, false),
		knobCard(t, "k1", "taf_1", 2),
		tafsCall(t, "c2", "version_install", map[string]any{"taf": "taf_1", "n": 2}),
		tafsResult(t, "c2", "version_install", `held for the owner`, false, false),
		tafsCall(t, "c3", "taf_dial", map[string]any{"taf": "Follow-up leads", "check": ""}),
		tafsResult(t, "c3", "taf_dial", `{"taf":"Follow-up leads","taf_id":"taf_1","version":3}`, false, false),
		tafsCall(t, "c4", "version_install", map[string]any{"taf": "taf_1", "n": 3}),
		tafsResult(t, "c4", "version_install", `held for the owner`, false, false),
		// Neither counts: a refused draft made no version, and an install sent beside a read never reached the owner.
		tafsCall(t, "c5", "taf_dial", map[string]any{"taf": "Follow-up leads"}),
		tafsResult(t, "c5", "taf_dial", `changing the dial is for the people who manage it`, true, false),
		tafsCall(t, "c6", "version_install", map[string]any{"taf": "taf_1", "n": 4}),
		tafsResult(t, "c6", "version_install", `not made`, true, true),
		// Results to a plain place make no version.
		tafsCall(t, "c7", "taf_results", map[string]any{"taf": "taf_1", "places": []string{"chat"}}),
		tafsResult(t, "c7", "taf_results", `{"taf":"Follow-up leads","results":["chat"]}`, false, false),
	}
	dials := dialsOf(events)
	// Read twice, as the harness reads each run: kept once.
	dials = keptDials(keptDials(nil, dials), dialsOf(events))
	if len(dials) != 5 {
		t.Fatalf("dials: %+v", dials)
	}

	sheets := connectAsk{Entry: "connector:managed:googlesheets", Grant: "grt_s"}
	clean := []string{"Done: your taf fills in Next step every Monday."}
	g := byName(gradeConnectBar(barRun{asks: []connectAsk{sheets}, approvals: []approvalSeen{{Grant: "grt_s"}, {Grant: "grt_s", Draft: "drf_1"}},
		statements: clean, dials: dials}))
	// The knob card and the install held for version 2 are one approval; version 3's install is another.
	if r := g[oneApprovalPerApp]; r.Pass || strings.Count(r.Why, "asked apart from the Card") != 2 ||
		!strings.Contains(r.Why, "dial version 2") || !strings.Contains(r.Why, "dial version 3") || strings.Contains(r.Why, "version 4") {
		t.Errorf("gate 1: %+v", r)
	}
	if r := g[noLoopAfterRefusal]; r.Pass || !strings.Contains(r.Why, "version 2 of the taf drafted during setup") ||
		!strings.Contains(r.Why, "version 3 of the taf drafted during setup") {
		t.Errorf("gate 3: %+v", r)
	}

	// The same setup with the dial in the Card's approval: nothing apart, no version drafted, both pass.
	g = byName(gradeConnectBar(barRun{asks: []connectAsk{sheets}, approvals: []approvalSeen{{Grant: "grt_s"}, {Grant: "grt_s", Draft: "drf_1"}}, statements: clean}))
	if !g[oneApprovalPerApp].Pass || !g[noLoopAfterRefusal].Pass {
		t.Errorf("the dial in the Card: %+v", g)
	}

	// A dial switched to with no app connected is still graded, not skipped.
	g = byName(gradeConnectBar(barRun{statements: clean, dials: dialsOf(events[2:3])}))
	if r := g[oneApprovalPerApp]; r.Pass || r.Skipped {
		t.Errorf("a knob alone: %+v", r)
	}
	if r := g[noLoopAfterRefusal]; !r.Pass {
		t.Errorf("a knob alone drafted nothing here: %+v", r)
	}
}
