package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// heldRefunds is a refunds setup whose agreed rules ask past €5 a refund and
// refuse past €50 a day, with the Cards KIFF holds as cardsHeldBy gives them.
func heldRefunds(t *testing.T, cards string) (said, []map[string]any) {
	t.Helper()
	rule := refundRule()
	rule.Over, rule.Limits[0].Over = "refuse", "ask"
	var held []map[string]any
	if err := json.Unmarshal([]byte(cards), &held); err != nil {
		t.Fatal(err)
	}
	return rulesSaid([]shownRule{rule, {Tool: "list_charges", Over: "refuse"}}, agreeConfirm("rules-1")), held
}

const agreedSplit = `[
	{"id": "each", "on_exceed": "owner", "grants": [{"action": "refund_charge_k3", "per_action_max": [{"quantity": "sum(amount)", "max": 500}]}], "aggregates": []},
	{"id": "day", "on_exceed": "refuse", "grants": [{"action": "refund_charge_k3"}], "aggregates": [{"quantity": "sum(amount)", "limit": 5000, "window": "calendar_day"}]},
	{"id": "reads", "on_exceed": "refuse", "grants": [{"action": "list_charges_k3"}], "aggregates": [{"quantity": "count", "limit": 10000, "window": "calendar_day"}]}]`

func TestTheCardsHoldOnlyTheAgreedFigures(t *testing.T) {
	if got := unagreedFigures(heldRefunds(t, agreedSplit)); len(got) != 0 {
		t.Fatalf("as agreed, the reads' count of KIFF's included: %v", got)
	}
	// Eval 43927756622, Setup run 1: an hour's €50 held on the per-refund Card.
	hour := strings.Replace(agreedSplit, `"max": 500}]}], "aggregates": []`, `"max": 500}]}], "aggregates": [{"quantity": "sum(amount)", "limit": 5000, "window": "rolling_1h"}]`, 1)
	if got := unagreedFigures(heldRefunds(t, hour)); len(got) != 1 || got[0] != "refund_charge_k3 sum(amount) per rolling_1h 5000" {
		t.Fatalf("the Eval's hour: %v", got)
	}
	// Any window, a count beside the amounts, and the tool in KIFF's case.
	for _, extra := range []string{
		`{"quantity": "sum(amount)", "limit": 1, "window": "lifetime"}`,
		`{"quantity": "sum(amount)", "limit": 1, "window": "ROLLING_7D"}`,
		`{"quantity": "count", "limit": 100, "window": "calendar_day"}`,
	} {
		cards := strings.Replace(agreedSplit, `"aggregates": [{"quantity": "sum(amount)", "limit": 5000, "window": "calendar_day"}]`,
			`"aggregates": [{"quantity": "sum(amount)", "limit": 5000, "window": "calendar_day"}, `+extra+`]`, 1)
		if got := unagreedFigures(heldRefunds(t, cards)); len(got) != 1 {
			t.Errorf("%s: %v", extra, got)
		}
	}
	// A total for each item (the judge's), and a count of 0 (each call asks): none.
	for _, kept := range []string{
		`{"quantity": "count", "limit": 1, "window": "lifetime", "per": "charge"}`,
		`{"quantity": "count", "limit": 0, "window": "calendar_day"}`,
	} {
		cards := strings.Replace(agreedSplit, `"max": 500}]}], "aggregates": []`, `"max": 500}]}], "aggregates": [`+kept+`]`, 1)
		if got := unagreedFigures(heldRefunds(t, cards)); len(got) != 0 {
			t.Errorf("%s: %v", kept, got)
		}
	}
	// A rule with no figures names none to compare.
	s, held := heldRefunds(t, hour)
	notice := s.cards["rules-1"]
	notice.Rules[0].Limits = nil
	s.cards["rules-1"] = notice
	if got := unagreedFigures(s, held); len(got) != 0 {
		t.Errorf("no figures: %v", got)
	}
}
