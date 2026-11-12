package cases

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/kiff/agents/run"
)

// The record's lines are tafs cloud's, line for line (kiff/tafs,
// runtime/test/delivery.test.ts, the same cases): KIFF's resource when it
// names one, else the call's own id-like arguments, never a message or an
// amount.
func TestTheRecordsLinesAreAsTafsCloudWritesThem(t *testing.T) {
	in := func(s string) json.RawMessage { return json.RawMessage(s) }
	for name, tt := range map[string]struct {
		done []run.Done
		want []string
	}{
		"a resource, cleaned and cut": {
			[]run.Done{{Tool: "a\nb", Resource: " "}, {Tool: ""}, {Tool: "c", Resource: strings.Repeat("x", 200)}},
			[]string{"- a b", "- c on " + strings.Repeat("x", 120)}},
		"an id-shaped argument, never the amount or the note": {
			[]run.Done{{Tool: "close_dispute", Input: in(`{"dispute":"T-2002","amount":4900,"note":"Accepted: small amount"}`)}},
			[]string{"- close_dispute (dispute: T-2002)"}},
		"an argument named as an id, a number too": {
			[]run.Done{{Tool: "send_invoice", Input: in(`{"invoice_id":12345,"customer":"cus_9","message":"Thanks!"}`)}},
			[]string{"- send_invoice (invoice_id: 12345)"}},
		"the input as a string of JSON, and two at most": {
			[]run.Done{{Tool: "refund", Input: in(`"{\"charge\":\"ch_1\",\"payment_intent\":\"pi_2\",\"order\":\"o_3\"}"`)}},
			[]string{"- refund (charge: ch_1, payment_intent: pi_2)"}},
		"nothing id-like, not JSON, not an object": {
			[]run.Done{{Tool: "post", Input: in(`{"text":"hello there"}`)}, {Tool: "x", Input: in(`"not json"`)}, {Tool: "y", Input: in(`[1]`)}, {Tool: "z"}},
			[]string{"- post", "- x", "- y", "- z"}},
		"KIFF's resource wins": {
			[]run.Done{{Tool: "close_dispute", Resource: "dp_1", Input: in(`{"dispute":"T-2002"}`)}},
			[]string{"- close_dispute on dp_1"}},
		"a long value isn't an id, and a named one over 64 isn't either": {
			[]run.Done{{Tool: "t", Input: in(`{"ref":"` + strings.Repeat("a1", 40) + `","thing_id":"` + strings.Repeat("b", 65) + `"}`)}},
			[]string{"- t"}},
		"named ids come before shaped ones": {
			[]run.Done{{Tool: "t", Input: in(`{"order":"o_3","chargeId":"ch_1"}`)}},
			[]string{"- t (chargeId: ch_1)"}},
		"a number is never id-shaped": {
			[]run.Done{{Tool: "t", Input: in(`{"count":12,"order":"T-9"}`)}},
			[]string{"- t (order: T-9)"}},
	} {
		if got := recordLines(tt.done); !slices.Equal(got, tt.want) {
			t.Errorf("%s: %q, want %q", name, got, tt.want)
		}
	}
	// Fifty calls at most.
	many := make([]run.Done, 60)
	for i := range many {
		many[i] = run.Done{Tool: "t"}
	}
	if got := recordLines(many); len(got) != 50 {
		t.Errorf("%d lines for 60 calls", len(got))
	}
}
