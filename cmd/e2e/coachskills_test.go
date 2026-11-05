package main

import (
	"encoding/json"
	"io"
	"testing"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool"
)

// A conversation opens with the skills tafs cloud would name for its entry
// point, as stored versions in the definition's order; a definition that names
// none of them (kiff/tafs#218's names, or none) sends none.
func TestStartSkillsFollowTafsCloudsTable(t *testing.T) {
	tt := &test{tenant: "t1", cfg: config{log: io.Discard},
		setupSkills:    []string{"cards-and-holds", "connect-an-app", "draft-the-card", "halting", "improve-a-taf", "name-the-tools", "set-up-a-taf", "the-dial", "the-tafs", "usage-and-plans"},
		setupSkillRefs: []string{"cards-and-holds@1", "connect-an-app@1", "draft-the-card@1", "halting@1", "improve-a-taf@1", "name-the-tools@2", "set-up-a-taf@1", "the-dial@1", "the-tafs@1", "usage-and-plans@1"}}
	want := map[string][]string{
		"free":    {"connect-an-app@1", "draft-the-card@1", "name-the-tools@2", "set-up-a-taf@1", "usage-and-plans@1"},
		"improve": {"connect-an-app@1", "draft-the-card@1", "improve-a-taf@1", "name-the-tools@2"},
		"other":   nil,
	}
	for entry, w := range want {
		got := tt.startSkills(entry)
		if len(got) != len(w) {
			t.Fatalf("%s: %v, want %v", entry, got, w)
		}
		for i := range w {
			if got[i] != w[i] {
				t.Fatalf("%s: %v, want %v", entry, got, w)
			}
		}
	}
	old := &test{tenant: "t1", cfg: config{log: io.Discard}, setupSkills: []string{"change-a-taf"}, setupSkillRefs: []string{"change-a-taf@1"}}
	if got := old.startSkills("free"); len(got) != 0 {
		t.Fatalf("an older definition sent %v", got)
	}
	if got := old.skillOf([]string{"improve-a-taf", "change-a-taf"}); got != "change-a-taf" {
		t.Fatalf("the question's skill by the older name: %q", got)
	}
	if got := tt.skillOf([]string{"cards-and-holds", "explain-a-hold"}); got != "cards-and-holds" {
		t.Fatalf("the question's skill by the new name: %q", got)
	}
}

// Setup states only the mode the connection reports: with test reported, a live
// mode or real money is a claim and test isn't; with none reported, any mode is.
func TestClaimsModeReadsWhatTheConnectionReports(t *testing.T) {
	test := []string{"Chosen: test mode. Up to 5 EUR a refund.", "Stripe in test mode, no real money.", "Reembolsos en modo de prueba, sin dinero real."}
	if c := claimsMode(test, true); c != "" {
		t.Fatalf("test mode, which the vendor reports, read as a claim: %q", c)
	}
	for _, claim := range []string{"Live mode (real money) from now on.", "Esto mueve dinero real.", "Refunds runs in live mode."} {
		if c := claimsMode([]string{"fine", claim}, true); c != claim {
			t.Fatalf("%q not read as a claim with test reported: %q", claim, c)
		}
	}
	for _, claim := range []string{"Desk in test mode.", "No real money moves here.", "Modo de prueba activado."} {
		if c := claimsMode([]string{claim}, false); c != claim {
			t.Fatalf("%q not read as a claim with no mode reported: %q", claim, c)
		}
	}
	if c := claimsMode([]string{"Up to 20 replies a day."}, false); c != "" {
		t.Fatalf("a text with no mode read as a claim: %q", c)
	}
}

// A test mode said before any tool result reported it is a claim; after a
// read that returned livemode false, it's what the connection reports
// (agentgabo on #319: the same sentence, before and after).
func TestModeCountsAsReportedOnlyAfterAToolSaidSo(t *testing.T) {
	ev := func(kind session.EventKind, body any) session.Event {
		b, _ := json.Marshal(body)
		return session.Event{Run: "r1", Kind: kind, Body: b}
	}
	say := ev(session.AgentMessage, agent.TextBody{Text: "Reembolsos en modo de prueba."})
	read := ev(session.ToolResult, agent.ToolResultBody{Tool: "read", Result: tool.Result{Text: `{"id":"ch_1","object":"charge","livemode": false}`}})
	accounts := ev(session.ToolResult, agent.ToolResultBody{Tool: "read", Result: tool.Result{Text: `{"accounts":[{"account":"acct_1","mode":"test"}]}`}})
	refused := ev(session.ToolResult, agent.ToolResultBody{Tool: "read", Result: tool.Result{Text: `"livemode": false`, IsError: true}})

	before, reported := beforeTestReport([]session.Event{say, read}, "r1", false)
	if len(before) != 1 || !reported {
		t.Fatalf("said before the read: %v %v", before, reported)
	}
	if c := claimsMode(before, false); c == "" {
		t.Fatal("a test mode said before any read wasn't a claim")
	}
	before, reported = beforeTestReport([]session.Event{read, say}, "r1", false)
	if len(before) != 0 || !reported {
		t.Fatalf("said after the read: %v %v", before, reported)
	}
	if _, r := beforeTestReport([]session.Event{accounts}, "r1", false); !r {
		t.Fatal("an accounts read with mode test didn't report it")
	}
	if b, r := beforeTestReport([]session.Event{refused, say}, "r1", false); r || len(b) != 1 {
		t.Fatalf("an error result reported test: %v %v", b, r)
	}
	// A later run keeps what an earlier one reported; another run's events don't count.
	if b, _ := beforeTestReport([]session.Event{say}, "r1", true); len(b) != 0 {
		t.Fatalf("a report from an earlier run was forgotten: %v", b)
	}
	other := read
	other.Run = "r2"
	if _, r := beforeTestReport([]session.Event{other}, "r1", false); r {
		t.Fatal("another run's read counted")
	}
}

// A new taf's result carries the job Setup drafted with the owner (kiff/tafs#476): an
// empty or custom job with a kickoff, instructions optional; a named job isn't one.
func TestOwnJobIsTheDraftedJob(t *testing.T) {
	for _, tc := range []struct {
		job, kickoff string
		want         bool
	}{
		{"", "Work the refund queue.", true},
		{"custom", "Work the refund queue.", true},
		{"", "", false},
		{"custom", "", false},
		{"refunds", "Work the refund queue.", false},
		{"billpay", "", false},
	} {
		if got := ownJob(tc.job, tc.kickoff); got != tc.want {
			t.Fatalf("%+v: %v", tc, got)
		}
	}
}
