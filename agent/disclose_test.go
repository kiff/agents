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
	// First in each of the fields the call carries: whichever the reader is shown starts with it.
	out = evidenceOf(t, evidence.First("update_dispute", json.RawMessage(`{"evidence":{"uncategorized_text":"Delivered on 2 Nov.","product_description":"Mug"}}`)))
	if out["uncategorized_text"] != line+"\n\nDelivered on 2 Nov." || out["product_description"] != line+"\n\nMug" {
		t.Fatalf("the line went %v", out)
	}
	// Evidence with none of the fields gets the first, with the line alone.
	out = evidenceOf(t, evidence.First("update_dispute", json.RawMessage(`{"evidence":{"shipping_carrier":"UPS"}}`)))
	if out["uncategorized_text"] != line {
		t.Fatalf("the line went %v", out)
	}
	// Said first already: the call is sent as the model wrote it, byte for byte.
	said := json.RawMessage(`{"submit": true, "evidence": {"product_description": "` + line + `\n\nMug."}}`)
	if got := evidence.First("update_dispute", said); string(got) != string(said) {
		t.Fatalf("a disclosed call changed: %s", got)
	}
	// Said elsewhere in the fields: moved to the top of each, said once.
	out = evidenceOf(t, evidence.First("update_dispute", json.RawMessage(`{"evidence":{"uncategorized_text":"Delivered. `+line+`","product_description":"Mug.\n\n`+line+`"}}`)))
	if out["uncategorized_text"] != line+"\n\nDelivered." || out["product_description"] != line+"\n\nMug." {
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
	// An html part's tags aren't its words, even English ones in an attribute.
	if got := language(`<span title="the order is for you and your thanks">Hola, gracias.</span>`); got != "es" {
		t.Errorf("tag words tipped the vote: %q", got)
	}
	// Its line in the text's language first already: as written. Any of them elsewhere: moved, in the text's language.
	es := "Hecho: te hemos devuelto el importe del pedido 1043."
	if got := body(d.Lines["es"] + "\n\n" + es); got != d.Lines["es"]+"\n\n"+es {
		t.Errorf("said first: %q", got)
	}
	if got := body(es + "\n\n" + d.Line); got != d.Lines["es"]+"\n\n"+es {
		t.Errorf("moved: %q", got)
	}
	// A line in another language at the top (the model copied the one it saw) is put in the text's.
	hola := "Hola, hemos recibido su pedido y el reembolso está en camino. Gracias por su paciencia."
	if got := body(d.Line + "\n\n" + hola); got != d.Lines["es"]+"\n\n"+hola {
		t.Errorf("English on a Spanish text: %q", got)
	}
	en := "Your refund of 42.00 EUR for order 1043 has been issued. Thanks!"
	if got := body(d.Lines["es"] + "\n\n" + en); got != d.Line+"\n\n"+en {
		t.Errorf("Spanish on an English text: %q", got)
	}
}

// An email's text body and its html are two renderings of one message, and
// a reader is shown one of them: each starts with the line, said once, in
// the message's language (the parts read together).
func TestTheDisclosureIsFirstInEachPartOfAMessage(t *testing.T) {
	d := &Disclosure{Line: "Written by an AI agent for Smith & Co.", In: []DisclosureIn{{Tool: "send_email", Fields: []string{"text", "html"}}}, Lines: map[string]string{
		"es": "Escrito por un agente de IA para Smith & Co.",
		"fr": "Rédigé par un agent d'IA pour Smith & Co.",
	}}
	l, es, fr := d.Line, d.Lines["es"], d.Lines["fr"]
	hi, hola := "Hi, your refund is on its way.", "Hola, hemos recibido su pedido y el reembolso está en camino. Gracias por su paciencia."
	// In the html part, the line is a paragraph of its own, escaped as html writes it.
	if htmlLine(l) != "<p>Written by an AI agent for Smith &amp; Co.</p>" {
		t.Fatalf("htmlLine %q", htmlLine(l))
	}
	page := "<!DOCTYPE html><html><head><title>Your refund</title></head><body style=\"margin:0\"><p>" + hi + "</p></body></html>"
	for _, c := range []struct{ name, text, html, wantText, wantHTML string }{
		{"the model labelled the html only", hi, "<p>" + l + "</p><p>" + hi + "</p>", l + "\n\n" + hi, htmlLine(l) + "<p>" + hi + "</p>"},
		{"the html's copy at its end", l + "\n\n" + hi, "<p>" + hi + "</p><p>" + l + "</p>", l + "\n\n" + hi, htmlLine(l) + "<p>" + hi + "</p>"},
		{"said twice in one part", l + "\n\n" + hi + "\n\n" + l, "<p>" + hi + "</p>", l + "\n\n" + hi, htmlLine(l) + "<p>" + hi + "</p>"},
		{"the html's copy as html writes it", hi, "<p>Written by an AI agent for Smith &amp; Co.</p><p>" + hi + "</p>", l + "\n\n" + hi, htmlLine(l) + "<p>" + hi + "</p>"},
		{"neither labelled", hi, "<p>" + hi + "</p>", l + "\n\n" + hi, htmlLine(l) + "<p>" + hi + "</p>"},
		{"an html part that starts inline", hi, "Hi, your refund is <b>on its way</b>.", l + "\n\n" + hi, htmlLine(l) + "Hi, your refund is <b>on its way</b>."},
		{"a whole page: just inside its body", hi, page,
			l + "\n\n" + hi, "<!DOCTYPE html><html><head><title>Your refund</title></head><body style=\"margin:0\">" + htmlLine(l) + "<p>" + hi + "</p></body></html>"},
		{"a page with no body: after its doctype, never before", hi, "<!doctype html><p>" + hi + "</p>", l + "\n\n" + hi, "<!doctype html>" + htmlLine(l) + "<p>" + hi + "</p>"},
		{"a Spanish message with the English line", l + "\n\n" + hola, "<p>" + l + "</p><p>" + hola + "</p>", es + "\n\n" + hola, htmlLine(es) + "<p>" + hola + "</p>"},
		{"a Spanish message labelled in French", fr + "\n\n" + hola, "<p>" + hola + "</p>", es + "\n\n" + hola, htmlLine(es) + "<p>" + hola + "</p>"},
		{"a text part too short to tell, read with its html", "Pedido 1043.", "<p>Pedido 1043.</p><p>" + hola + "</p>", es + "\n\nPedido 1043.", htmlLine(es) + "<p>Pedido 1043.</p><p>" + hola + "</p>"},
		{"a quoted earlier message keeps its line", "Thanks!\n\n> " + l + "\n> Your order shipped.", "<p>Thanks!</p><blockquote><p>" + l + "</p><p>Your order shipped.</p></blockquote>",
			l + "\n\nThanks!\n\n> " + l + "\n> Your order shipped.", htmlLine(l) + "<p>Thanks!</p><blockquote><p>" + l + "</p><p>Your order shipped.</p></blockquote>"},
	} {
		in, _ := json.Marshal(map[string]string{"text": c.text, "html": c.html})
		var out map[string]string
		if err := json.Unmarshal(d.First("send_email", in), &out); err != nil {
			t.Fatal(err)
		}
		if out["text"] != c.wantText || out["html"] != c.wantHTML {
			t.Errorf("%s:\ntext %q\nhtml %q", c.name, out["text"], out["html"])
		}
	}
}

// A field whose name doesn't say html but whose text is a whole page is an
// html part too; a plain text field with a "<" in it isn't.
func TestAWholePageIsAnHTMLPart(t *testing.T) {
	d := &Disclosure{Line: line, In: []DisclosureIn{{Tool: "send_email", Fields: []string{"body"}}}}
	body := func(text string) string {
		t.Helper()
		in, _ := json.Marshal(map[string]string{"body": text})
		var out map[string]string
		if err := json.Unmarshal(d.First("send_email", in), &out); err != nil {
			t.Fatal(err)
		}
		return out["body"]
	}
	if got := body("<html><body><p>Hi.</p></body></html>"); got != "<html><body>"+htmlLine(line)+"<p>Hi.</p></body></html>" {
		t.Errorf("a page: %q", got)
	}
	if got := body("Mug <350 ml> & lid"); got != line+"\n\nMug <350 ml> & lid" {
		t.Errorf("text with a <: %q", got)
	}
}

// A call that went through First goes through it again unchanged, byte for
// byte: the line is put once, and the rest of the text is as the model wrote
// it (its spaces and blank runs included).
func TestTheDisclosureIsPutOnce(t *testing.T) {
	d := &Disclosure{Line: line, In: []DisclosureIn{{Tool: "send_email", Fields: []string{"text", "html"}}}, Lines: map[string]string{
		"es": "Escrito por un agente de IA para la empresa.",
		"fr": "Rédigé par un agent d'IA pour l'entreprise.",
	}}
	for _, in := range []string{
		`{"text":"Hi,\n\n\nyour refund is on its way.  \n","html":"<p>Hi,</p>\n\n\n<p>your refund is on its way.</p>"}`,
		`{"text":"Hola, hemos recibido su pedido y el reembolso está en camino. ` + line + `"}`,
		`{"text":"Votre remboursement de 42,00 EUR pour la commande 1043 a été effectué. Merci !","html":"<p>` + line + `</p><p>Votre remboursement a été effectué.</p>"}`,
		`{"subject":"Your refund"}`,
		`{"text":"` + line + `"}`,
		`{"html":"<!DOCTYPE html><html><body class=\"m\"><p>Hola, hemos recibido su pedido y el reembolso está en camino.</p></body></html>"}`,
		`{"text":"Thanks!\n\n> ` + line + `\n> Your order shipped.","html":"<blockquote><p>` + line + `</p></blockquote>"}`,
	} {
		once := d.First("send_email", json.RawMessage(in))
		if twice := d.First("send_email", once); string(twice) != string(once) {
			t.Errorf("a second pass changed %s:\n%s\n%s", in, once, twice)
		}
	}
}

// without takes each line out with the space around it, and leaves the rest of the text as it was.
func TestWithoutTakesOnlyTheLineOut(t *testing.T) {
	for _, c := range []struct{ text, want string }{
		{line + "\n\nHi.", "Hi."},
		{"Hi.\n\n" + line, "Hi."},
		{"Hi.\n\n" + line + "\nBye.", "Hi.\n\nBye."},
		{"Delivered. " + line + " More.", "Delivered. More."},
		{"<p>" + line + "</p><p>Hi.</p>", "<p></p><p>Hi.</p>"},
		{"  " + line + "  ", ""},
		{"Hi,\n\n\nno line here.  \n", "Hi,\n\n\nno line here.  \n"},
		{line + line, ""},
		{"Thanks.\n\n> " + line + "\n> Your order shipped.", "Thanks.\n\n> " + line + "\n> Your order shipped."},
		{line + "\n\nThanks.\n\n> " + line, "Thanks.\n\n> " + line},
		{"<blockquote>" + line + "</blockquote><p>" + line + "</p>", "<blockquote>" + line + "</blockquote><p></p>"},
		// Nested: the outer quote goes on after the inner one closes, and keeps its line there too.
		{"<blockquote><p>Earlier.</p><blockquote><p>Older.</p></blockquote><p>" + line + "</p></blockquote>",
			"<blockquote><p>Earlier.</p><blockquote><p>Older.</p></blockquote><p>" + line + "</p></blockquote>"},
	} {
		if got := without(c.text, []string{line}); got != c.want {
			t.Errorf("without(%q) = %q, want %q", c.text, got, c.want)
		}
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
