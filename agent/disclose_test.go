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
// and no word that an AI agent wrote it), and it goes first: whoever reads
// the message reads that an AI agent wrote it before anything else (AI Act
// Art. 50(1)).
func TestTheDisclosureGoesFirstInTheCall(t *testing.T) {
	// That run's evidence: no free text, so the line heads the product description.
	in := json.RawMessage(`{"dispute":"dp_1","evidence":{"product_description":"Blue mug, 350 ml","shipping_tracking_number":"1Z999","shipping_carrier":"UPS"},"submit":true}`)
	out := evidenceOf(t, evidence.First("update_dispute", in))
	if out["product_description"] != line+"\n\nBlue mug, 350 ml" || out["shipping_tracking_number"] != "1Z999" || out["shipping_carrier"] != "UPS" {
		t.Fatalf("the line went %v", out)
	}
	// The free text first, when the call carries it.
	out = evidenceOf(t, evidence.First("update_dispute", json.RawMessage(`{"evidence":{"uncategorized_text":"Delivered on 2 Nov.","product_description":"Mug"}}`)))
	if out["uncategorized_text"] != line+"\n\nDelivered on 2 Nov." || out["product_description"] != "Mug" {
		t.Fatalf("the line went %v", out)
	}
	// Evidence with none of the fields gets the first, with the line alone.
	out = evidenceOf(t, evidence.First("update_dispute", json.RawMessage(`{"evidence":{"shipping_carrier":"UPS"}}`)))
	if out["uncategorized_text"] != line {
		t.Fatalf("the line went %v", out)
	}
	// Said first already: the call is sent as the model wrote it.
	said := json.RawMessage(`{"evidence":{"product_description":"` + line + `\n\nMug."}}`)
	if got := evidence.First("update_dispute", said); string(got) != string(said) {
		t.Fatalf("a disclosed call changed: %s", got)
	}
	// Said elsewhere, in any of the fields: moved to the top of the first, said once.
	out = evidenceOf(t, evidence.First("update_dispute", json.RawMessage(`{"evidence":{"uncategorized_text":"Delivered. `+line+`","product_description":"Mug.\n\n`+line+`"}}`)))
	if out["uncategorized_text"] != line+"\n\nDelivered." || out["product_description"] != "Mug." {
		t.Fatalf("the line went %v", out)
	}
	// A call that sends nothing outside (submitting what was sent before) is as it was.
	submit := json.RawMessage(`{"dispute":"dp_1","submit":true}`)
	if got := evidence.First("update_dispute", submit); string(got) != string(submit) {
		t.Fatalf("a submit alone changed: %s", got)
	}
	// The tool as KIFF names it for a connection, and no other tool.
	for _, name := range []string{"update_dispute_k3f9", "stripe_update_dispute"} {
		if out := evidenceOf(t, evidence.First(name, in)); !strings.HasPrefix(out["product_description"].(string), line) {
			t.Fatalf("%s: %v", name, out)
		}
	}
	if got := evidence.First("close_dispute", in); string(got) != string(in) {
		t.Fatalf("another tool's call changed: %s", got)
	}
	// Only one word of a connection or a vendor: never a tool whose name only starts or ends the same.
	for _, other := range []string{"update_dispute_evidence_files", "bulk_update_dispute_all", "update_disputes"} {
		if got := evidence.First(other, in); string(got) != string(in) {
			t.Fatalf("%s took the disclosure: %s", other, got)
		}
	}
	// Text is sent as written, with no HTML escapes.
	if got := evidence.First("update_dispute", json.RawMessage(`{"evidence":{"product_description":"Mug <350 ml> & lid"}}`)); !strings.Contains(string(got), "Mug <350 ml> & lid") {
		t.Fatalf("escaped: %s", got)
	}
	// An amount stays exactly as written.
	if got := evidence.First("update_dispute", json.RawMessage(`{"amount":12345678901234567890,"evidence":{"product_description":"Mug"}}`)); !strings.Contains(string(got), `"amount":12345678901234567890`) {
		t.Fatalf("the amount changed: %s", got)
	}
	// No disclosure set: nothing changes.
	var none *Disclosure
	if got := none.First("update_dispute", in); string(got) != string(in) {
		t.Fatalf("no disclosure changed the call: %s", got)
	}
}

// The line heads the text in the text's own language, when the disclosure
// has its line in it: a Spanish reply reads "Escrito por un agente de IA…"
// first. A language it lacks, or a text too short to tell, gets Line.
func TestTheDisclosureIsInTheTextsLanguage(t *testing.T) {
	d := &Disclosure{Line: "Written by an AI agent for Acme.", In: []DisclosureIn{{Tool: "send_reply", Fields: []string{"body"}}}, Lines: map[string]string{
		"es": "Escrito por un agente de IA para Acme.",
		"fr": "Rédigé par un agent d'IA pour Acme.",
		"de": "Von einem KI-Agenten für Acme geschrieben.",
		"pt": "Escrito por um agente de IA para Acme.",
		"it": "Scritto da un agente di IA per Acme.",
	}}
	if err := d.Check(); err != nil {
		t.Fatal(err)
	}
	body := func(text string) string {
		t.Helper()
		in, _ := json.Marshal(map[string]string{"body": text})
		var out map[string]string
		if err := json.Unmarshal(d.First("send_reply", in), &out); err != nil {
			t.Fatal(err)
		}
		return out["body"]
	}
	for _, c := range []struct{ lang, text string }{
		{"es", "Hecho: te hemos devuelto 42,00 EUR del pedido 1043 a tu tarjeta original. Llegará en 5–10 días."},
		{"fr", "Votre remboursement de 42,00 EUR pour la commande 1043 a été effectué. Merci !"},
		{"de", "Ihre Erstattung von 42,00 EUR für die Bestellung 1043 ist erledigt. Danke!"},
		{"pt", "Seu reembolso de 42,00 EUR do pedido 1043 foi feito. Obrigado!"},
		{"it", "Il rimborso di 42,00 EUR per l'ordine 1043 è stato fatto. Grazie!"},
	} {
		if got := body(c.text); got != d.Lines[c.lang]+"\n\n"+c.text {
			t.Errorf("%s: %q", c.lang, got)
		}
	}
	for _, text := range []string{
		"Your refund of 42.00 EUR for order 1043 has been issued. Thanks!",
		"OK",
		"Gracias.",                      // one word is too little to tell
		"Hola, gracias. Olá, obrigado.", // as much of two languages: neither
		"Teie tagasimakse on tehtud.",   // Estonian: no line for it here
	} {
		if got := body(text); got != d.Line+"\n\n"+text {
			t.Errorf("%q: %q", text, got)
		}
	}
	// Any of its lines first already: as written. Any of them elsewhere: moved, in the text's language.
	if got := body(d.Lines["es"] + "\n\nHecho."); got != d.Lines["es"]+"\n\nHecho." {
		t.Errorf("said first: %q", got)
	}
	es := "Hecho: te hemos devuelto el importe del pedido 1043."
	if got := body(es + "\n\n" + d.Line); got != d.Lines["es"]+"\n\n"+es {
		t.Errorf("moved: %q", got)
	}
}

func TestADisclosureIsChecked(t *testing.T) {
	many := map[string]string{}
	for _, code := range strings.Fields("aa ab ac ad ae af ag ah ai aj ak al am") {
		many[code] = line
	}
	for _, bad := range []*Disclosure{
		{Line: "", In: evidence.In},
		{Line: "two\nlines", In: evidence.In},
		{Line: line},
		{Line: line, In: []DisclosureIn{{Tool: "update_dispute"}}},
		{Line: line, In: []DisclosureIn{{Tool: "update_dispute", Fields: []string{"evidence..text"}}}},
		{Line: line, In: evidence.In, Lines: map[string]string{"spa": line}},
		{Line: line, In: evidence.In, Lines: map[string]string{"ES": line}},
		{Line: line, In: evidence.In, Lines: map[string]string{"es": ""}},
		{Line: line, In: evidence.In, Lines: map[string]string{"es": "dos\nlíneas"}},
		{Line: line, In: evidence.In, Lines: map[string]string{"es": strings.Repeat("a", MaxDisclosureLine+1)}},
		{Line: line, In: evidence.In, Lines: many},
	} {
		if bad.Check() == nil {
			t.Errorf("passed: %+v", bad)
		}
	}
	if err := evidence.Check(); err != nil {
		t.Fatal(err)
	}
}
