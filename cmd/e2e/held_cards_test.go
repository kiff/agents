package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The judge reads a taf's Cards with every term a draft names: a total
// counted for each order, ever, isn't shown as one in the taf's whole life
// (the billing setup's "signs what was agreed", 13 Nov 2026).
func TestHeldCardsKeepATotalsPerAndTheCardsEnd(t *testing.T) {
	var listed any
	if err := json.Unmarshal([]byte(`[
		{"id": "invoices-drafts", "subject": "invoices", "subjects": ["invoices"], "active": true, "domain": "gateway",
		 "on_exceed": "owner", "valid_until": "2026-11-19T00:00:00Z", "location": "Europe/Paris", "hold_expiry_seconds": 600,
		 "grants": [{"action": "create_invoice", "title": "Create an invoice"}],
		 "aggregates": [{"quantity": "count", "quantity_of": "calls", "limit": 10, "window": "calendar_day", "measure": "effects"},
		                {"quantity": "count", "quantity_of": "calls", "limit": 1, "window": "lifetime", "per": "order", "measure": "effects"}]},
		{"id": "someone-elses", "subject": "other", "active": true, "grants": [], "aggregates": []}
	]`), &listed); err != nil {
		t.Fatal(err)
	}
	got := cardsHeldBy(listed, "invoices")
	if len(got) != 1 {
		t.Fatalf("the taf's own Cards only: %v", got)
	}
	b, _ := json.Marshal(got[0])
	for _, want := range []string{
		`{"limit":1,"measure":"effects","per":"order","quantity":"count","window":"lifetime"}`,
		`"valid_until":"2026-11-19T00:00:00Z"`, `"location":"Europe/Paris"`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the Card lost %s: %s", want, b)
		}
	}
	// What KIFF's view adds, a draft never names.
	for _, extra := range []string{"title", "quantity_of", "hold_expiry_seconds", "subjects"} {
		if strings.Contains(string(b), `"`+extra+`"`) {
			t.Errorf("the Card has KIFF's view's %s: %s", extra, b)
		}
	}
	// And the judge is told how to read a per.
	if !strings.Contains(agreedRule, "per: order} is one per order, ever") {
		t.Error("the rule doesn't say how a total for each item reads")
	}
}
