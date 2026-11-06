package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

const line = "Written by an AI agent for the company."

var evidence = &Disclosure{Line: line, In: []DisclosureIn{{Tool: "update_dispute", Fields: []string{"evidence.uncategorized_text", "evidence.product_description"}}}}

func evidenceOf(t *testing.T, in json.RawMessage) map[string]any {
	t.Helper()
	var call struct {
		Evidence map[string]any `json:"evidence"`
	}
	if err := json.Unmarshal(in, &call); err != nil {
		t.Fatal(err)
	}
	return call.Evidence
}

// The line goes in the call whatever the model wrote (Eval 42193895936: the
// evidence carried a product description, a tracking number and a carrier,
// and no word that an AI agent wrote it).
func TestTheDisclosureGoesInTheCall(t *testing.T) {
	// That run's evidence: no free text, so the line ends the product description.
	in := json.RawMessage(`{"dispute":"dp_1","evidence":{"product_description":"Blue mug, 350 ml","shipping_tracking_number":"1Z999","shipping_carrier":"UPS"},"submit":true}`)
	out := evidenceOf(t, evidence.Into("update_dispute", in))
	if out["product_description"] != "Blue mug, 350 ml\n\n"+line || out["shipping_tracking_number"] != "1Z999" || out["shipping_carrier"] != "UPS" {
		t.Fatalf("the line went %v", out)
	}
	// The free text first, when the call carries it.
	out = evidenceOf(t, evidence.Into("update_dispute", json.RawMessage(`{"evidence":{"uncategorized_text":"Delivered on 2 Nov.","product_description":"Mug"}}`)))
	if out["uncategorized_text"] != "Delivered on 2 Nov.\n\n"+line || out["product_description"] != "Mug" {
		t.Fatalf("the line went %v", out)
	}
	// Evidence with none of the fields gets the first, with the line alone.
	out = evidenceOf(t, evidence.Into("update_dispute", json.RawMessage(`{"evidence":{"shipping_carrier":"UPS"}}`)))
	if out["uncategorized_text"] != line {
		t.Fatalf("the line went %v", out)
	}
	// Said already, anywhere it goes: the call is sent as the model wrote it.
	said := json.RawMessage(`{"evidence":{"product_description":"Mug. ` + line + `"}}`)
	if got := evidence.Into("update_dispute", said); string(got) != string(said) {
		t.Fatalf("a disclosed call changed: %s", got)
	}
	// A call that sends nothing outside (submitting what was sent before) is as it was.
	submit := json.RawMessage(`{"dispute":"dp_1","submit":true}`)
	if got := evidence.Into("update_dispute", submit); string(got) != string(submit) {
		t.Fatalf("a submit alone changed: %s", got)
	}
	// The tool as KIFF names it for a connection, and no other tool.
	for _, name := range []string{"update_dispute_k3f9", "stripe_update_dispute"} {
		if out := evidenceOf(t, evidence.Into(name, in)); !strings.HasSuffix(out["product_description"].(string), line) {
			t.Fatalf("%s: %v", name, out)
		}
	}
	if got := evidence.Into("close_dispute", in); string(got) != string(in) {
		t.Fatalf("another tool's call changed: %s", got)
	}
	// An amount stays exactly as written.
	if got := evidence.Into("update_dispute", json.RawMessage(`{"amount":12345678901234567890,"evidence":{"product_description":"Mug"}}`)); !strings.Contains(string(got), `"amount":12345678901234567890`) {
		t.Fatalf("the amount changed: %s", got)
	}
	// No disclosure set: nothing changes.
	var none *Disclosure
	if got := none.Into("update_dispute", in); string(got) != string(in) {
		t.Fatalf("no disclosure changed the call: %s", got)
	}
}

func TestADisclosureIsChecked(t *testing.T) {
	for _, bad := range []*Disclosure{
		{Line: "", In: evidence.In},
		{Line: "two\nlines", In: evidence.In},
		{Line: line},
		{Line: line, In: []DisclosureIn{{Tool: "update_dispute"}}},
		{Line: line, In: []DisclosureIn{{Tool: "update_dispute", Fields: []string{"evidence..text"}}}},
	} {
		if bad.Check() == nil {
			t.Errorf("passed: %+v", bad)
		}
	}
	if err := evidence.Check(); err != nil {
		t.Fatal(err)
	}
}
