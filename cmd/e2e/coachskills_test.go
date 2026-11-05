package main

import (
	"io"
	"testing"
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
