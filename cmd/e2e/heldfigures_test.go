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
	{"id": "each", "on_exceed": "owner", "grants": [{"action": "refund_charge_k3", "per_action_max": [{"quantity": "sum(amount)", "max": 500}]}], "aggregates": [{"quantity": "count", "limit": 10000, "window": "calendar_day"}]},
	{"id": "day", "on_exceed": "refuse", "grants": [{"action": "refund_charge_k3"}], "aggregates": [{"quantity": "sum(amount)", "limit": 5000, "window": "calendar_day"}]},
	{"id": "reads", "on_exceed": "refuse", "grants": [{"action": "list_charges_k3"}], "aggregates": [{"quantity": "count", "limit": 10000, "window": "calendar_day"}]}]`

// Eval 43975057022, Setup run 3: KIFF refuses a Card with no total, so the
// held per-refund Card carries KIFF's own count, beside the refused Card with
// the day's total. That's the agreed rule as KIFF takes it, never an extra
// figure; the same count anywhere else is one.
func TestKIFFsOwnCountOnTheHeldCardIsNoFigureOfTheOwners(t *testing.T) {
	if got := unagreedFigures(heldRefunds(t, agreedSplit)); len(got) != 0 {
		t.Fatalf("the two Cards as KIFF takes them: %v", got)
	}
	kiffs := `{"quantity": "count", "limit": 10000, "window": "calendar_day"}`
	for name, cards := range map[string]string{
		// Alone: nothing bounds the refunds' amount, so 10,000 a day under the cap would pass unasked.
		"alone": `[{"id": "each", "on_exceed": "owner", "grants": [{"action": "refund_charge_k3", "per_action_max": [{"quantity": "sum(amount)", "max": 500}]}], "aggregates": [` + kiffs + `]}]`,
		// Beside a total for each customer only: one item's, so nothing bounds the day.
		"per item": strings.Replace(agreedSplit, `"aggregates": [{"quantity": "sum(amount)", "limit": 5000, "window": "calendar_day"}]`,
			`"aggregates": [{"quantity": "sum(amount)", "limit": 5000, "window": "calendar_day", "per": "customer"}]`, 1),
		// On the refused Card, beside its day's total.
		"refused": strings.Replace(agreedSplit, `"window": "calendar_day"}]},
	{"id": "reads"`, `"window": "calendar_day"}, `+kiffs+`]},
	{"id": "reads"`, 1),
		// Beside another total on the held Card.
		"beside": strings.Replace(agreedSplit, kiffs+`]},
	{"id": "day"`, kiffs+`, {"quantity": "sum(amount)", "limit": 1, "window": "rolling_1h"}]},
	{"id": "day"`, 1),
		// Another figure or window.
		"9999": strings.Replace(agreedSplit, `"limit": 10000, "window": "calendar_day"}]},
	{"id": "day"`, `"limit": 9999, "window": "calendar_day"}]},
	{"id": "day"`, 1),
		"24h": strings.Replace(agreedSplit, `"limit": 10000, "window": "calendar_day"}]},
	{"id": "day"`, `"limit": 10000, "window": "rolling_24h"}]},
	{"id": "day"`, 1),
	} {
		if got := unagreedFigures(heldRefunds(t, cards)); len(got) == 0 {
			t.Errorf("%s: KIFF's count let through: %s", name, cards)
		}
	}
}

func TestTheCardsHoldOnlyTheAgreedFigures(t *testing.T) {
	if got := unagreedFigures(heldRefunds(t, agreedSplit)); len(got) != 0 {
		t.Fatalf("as agreed, the reads' count of KIFF's included: %v", got)
	}
	// Eval 43927756622, Setup run 1: an hour's €50 held on the per-refund Card.
	hour := strings.Replace(agreedSplit, `"max": 500}]}], "aggregates": [{"quantity": "count", "limit": 10000, "window": "calendar_day"}]`, `"max": 500}]}], "aggregates": [{"quantity": "sum(amount)", "limit": 5000, "window": "rolling_1h"}]`, 1)
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
		cards := strings.Replace(agreedSplit, `"max": 500}]}], "aggregates": [{"quantity": "count", "limit": 10000, "window": "calendar_day"}]`, `"max": 500}]}], "aggregates": [`+kept+`]`, 1)
		if got := unagreedFigures(heldRefunds(t, cards)); len(got) != 0 {
			t.Errorf("%s: %v", kept, got)
		}
	}
	// A rule that names a total for each item names them all: the same quantity, window and per, or it's unagreed.
	perRule := func(cards string) []string {
		s, held := heldRefunds(t, cards)
		notice := s.cards["rules-1"]
		one := int64(1)
		notice.Rules[0].Limits = append(notice.Rules[0].Limits, shownLimit{Quantity: "count", Window: "lifetime", Limit: &one, Per: "charge"})
		s.cards["rules-1"] = notice
		return unagreedFigures(s, held)
	}
	withPer := func(agg string) string {
		return strings.Replace(agreedSplit, `"max": 500}]}], "aggregates": [{"quantity": "count", "limit": 10000, "window": "calendar_day"}]`, `"max": 500}]}], "aggregates": [`+agg+`]`, 1)
	}
	if got := perRule(withPer(`{"quantity": "count", "limit": 1, "window": "lifetime", "per": " Charge"}`)); len(got) != 0 {
		t.Errorf("the agreed per total: %v", got)
	}
	for agg, want := range map[string]string{
		`{"quantity": "count", "limit": 1, "window": "lifetime", "per": "customer"}`:     "refund_charge_k3 count per lifetime for each customer 1",
		`{"quantity": "count", "limit": 1, "window": "rolling_7d", "per": "charge"}`:     "refund_charge_k3 count per rolling_7d for each charge 1",
		`{"quantity": "sum(amount)", "limit": 1, "window": "lifetime", "per": "charge"}`: "refund_charge_k3 sum(amount) per lifetime for each charge 1",
	} {
		if got := perRule(withPer(agg)); len(got) != 1 || got[0] != want {
			t.Errorf("%s: %v, want %q", agg, got, want)
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
