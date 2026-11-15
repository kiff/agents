package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func limit(n int64) *int64 { return &n }

// rulesSaid is a refunds setup: a rules notice with its rules as data, then
// the confirm that agrees to it, over a connection with a read and a refund.
func rulesSaid(rules []shownRule, confirm component) said {
	notice := component{Type: "notice", ID: "rules-1", Title: "Your taf's rules", Text: "Refunds of €5 or less, €50 a day; above that it asks you.", Rules: rules}
	return said{
		cards: map[string]component{"rules-1": notice, confirm.ID: confirm},
		order: []string{"rules-1", confirm.ID},
		connected: []grantTool{
			{Tool: "list_charges", Name: "list_charges_k3", ReadOnly: true},
			{Tool: "refund_charge", Name: "refund_charge_k3", AmountArg: "amount"},
		},
	}
}

func refundRule() shownRule {
	return shownRule{Tool: "refund_charge", Over: "ask", Limits: []shownLimit{
		{Quantity: "sum(amount)", Window: "call", Limit: limit(500)},
		{Quantity: "sum(amount)", Window: "calendar_day", Limit: limit(5000)},
	}}
}

func agreeConfirm(agrees string) component {
	return component{Type: "confirm", ID: "agree-1", Agrees: agrees, Options: []option{{Value: "agree", Label: "Agree"}, {Value: "change", Label: "Change something"}}}
}

func TestTheAgreedRulesAreSentAsData(t *testing.T) {
	if r := gradeRulesAsData(rulesSaid([]shownRule{refundRule()}, agreeConfirm("rules-1"))); !r.Grade.Pass || r.Name != rulesAsData {
		t.Fatalf("rules as data, agreed by id: %+v", r)
	}
	// A rule for a read is fine beside them; the read needs none.
	read := shownRule{Tool: "list_charges", Over: "refuse"}
	if r := gradeRulesAsData(rulesSaid([]shownRule{refundRule(), read}, agreeConfirm("rules-1"))); !r.Grade.Pass {
		t.Fatalf("a read's rule too: %+v", r)
	}
	// The rule may name the tool by its KIFF name.
	byKiff := refundRule()
	byKiff.Tool = "Refund_Charge_k3"
	if r := gradeRulesAsData(rulesSaid([]shownRule{byKiff}, agreeConfirm("rules-1"))); !r.Grade.Pass {
		t.Fatalf("by KIFF's name: %+v", r)
	}
}

// A figure may carry its own over: ask past €5 a refund, refuse past €50 a day.
func TestEachFigureMaySayItsOwnOver(t *testing.T) {
	mixed := refundRule()
	mixed.Over = "refuse"
	mixed.Limits[0].Over = "ask"
	if r := gradeRulesAsData(rulesSaid([]shownRule{mixed}, agreeConfirm("rules-1"))); !r.Grade.Pass {
		t.Fatalf("ask on the call's figure, refuse by default: %+v", r.Grade)
	}
	var notice component
	if err := json.Unmarshal([]byte(`{"type":"notice","id":"n","rules":[{"tool":"create_refund","over":"refuse","limits":[{"quantity":"sum(amount)","window":"call","limit":500,"over":"ask"},{"quantity":"sum(amount)","window":"calendar_day","limit":5000}]}]}`), &notice); err != nil || notice.Rules[0].Limits[0].Over != "ask" || notice.Rules[0].Limits[1].Over != "" {
		t.Fatalf("a figure's own over isn't read: %v %+v", err, notice.Rules)
	}
	bad := refundRule()
	bad.Limits[1].Over = "owner"
	if r := gradeRulesAsData(rulesSaid([]shownRule{bad}, agreeConfirm("rules-1"))); r.Grade.Pass || !strings.Contains(r.Grade.Why, `sum(amount) per calendar_day says over "owner"`) {
		t.Errorf("an unknown over on a figure: %+v", r.Grade)
	}
}

func TestRulesInWordsOnlyOrUnnamedFail(t *testing.T) {
	noFigures := refundRule()
	noFigures.Limits = nil
	badOver := refundRule()
	badOver.Over = "owner"
	other := refundRule()
	other.Tool = "send_reply"
	for name, c := range map[string]struct {
		s    said
		want string
	}{
		"words only":        {rulesSaid(nil, agreeConfirm("rules-1")), "carries no rules, only words"},
		"a write unruled":   {rulesSaid([]shownRule{other}, agreeConfirm("rules-1")), "refund_charge_k3 has no rule"},
		"money, no figures": {rulesSaid([]shownRule{noFigures}, agreeConfirm("rules-1")), "moves money and its rule has no figures"},
		"over unknown":      {rulesSaid([]shownRule{badOver}, agreeConfirm("rules-1")), `over "owner"`},
		"confirm unnamed":   {rulesSaid([]shownRule{refundRule()}, agreeConfirm("")), "doesn't name it (agrees)"},
		"another notice":    {rulesSaid([]shownRule{refundRule()}, agreeConfirm("rules-0")), `agrees to "rules-0"`},
		"no agree option": {rulesSaid([]shownRule{refundRule()}, component{Type: "confirm", ID: "agree-1", Agrees: "rules-1",
			Options: []option{{Value: "yes", Label: "Yes"}}}), "no option with value agree"},
	} {
		r := gradeRulesAsData(c.s)
		if r.Grade.Pass || r.Grade.Skipped || !strings.Contains(r.Grade.Why, c.want) {
			t.Errorf("%s: %+v, want a failure saying %q", name, r.Grade, c.want)
		}
	}
	// Agreed on the Card's own approval, with no confirm: tafs cloud reads no agreement there.
	s := rulesSaid([]shownRule{refundRule()}, component{Type: "approval", ID: "agree-1", Draft: "drf_1"})
	if r := gradeRulesAsData(s); r.Grade.Pass || !strings.Contains(r.Grade.Why, "not a confirm") {
		t.Errorf("agreed on the approval: %+v", r.Grade)
	}
}

func TestRulesAsDataSkipsWithNothingToHold(t *testing.T) {
	reads := rulesSaid(nil, agreeConfirm("rules-1"))
	reads.connected = reads.connected[:1]
	if r := gradeRulesAsData(reads); !r.Grade.Skipped {
		t.Errorf("reads only: %+v", r.Grade)
	}
	none := rulesSaid([]shownRule{refundRule()}, agreeConfirm("rules-1"))
	none.order = []string{"agree-1"}
	if r := gradeRulesAsData(none); !r.Grade.Skipped {
		t.Errorf("no notice: %+v", r.Grade)
	}
	// The last agreed notice is the one held: an earlier one with rules doesn't count for a later one without.
	later := rulesSaid([]shownRule{refundRule()}, agreeConfirm("rules-1"))
	later.cards["rules-2"] = component{Type: "notice", ID: "rules-2", Text: "Changed: €10 a refund."}
	later.cards["agree-2"] = agreeConfirm("rules-2")
	later.order = append(later.order, "rules-2", "agree-2")
	if r := gradeRulesAsData(later); r.Grade.Pass || !strings.Contains(r.Grade.Why, "only words") {
		t.Errorf("a later notice in words only: %+v", r.Grade)
	}
}

func TestTheAgreedFiguresAreInTheWords(t *testing.T) {
	s := rulesSaid([]shownRule{refundRule()}, agreeConfirm("rules-1"))
	if r := figuresUnsaid(s); !r.Grade.Pass || !r.Advisory {
		t.Fatalf("€5 and €50 said: %+v", r)
	}
	off := refundRule()
	*off.Limits[0].Limit = 700
	s = rulesSaid([]shownRule{off}, agreeConfirm("rules-1"))
	if r := figuresUnsaid(s); r.Grade.Pass || !strings.Contains(r.Grade.Why, "refund_charge sum(amount) per call 700") {
		t.Fatalf("€7 not said: %+v", r.Grade)
	}
	for text, want := range map[string]float64{"5,00 €": 5, "1.000": 1000, "1 000,50": 1000.5, "€50": 50, "1'250": 1250, "1\t000": 1000} {
		if !numbersIn(text)[want] {
			t.Errorf("%q doesn't read %v: %v", text, want, numbersIn(text))
		}
	}
	// A number never runs across a line break: each line's own is read.
	if got := numbersIn("€1 000\n50 a day"); !got[1000] || !got[50] || got[100050] {
		t.Errorf("across a line break: %v", got)
	}
	s = rulesSaid([]shownRule{refundRule()}, agreeConfirm("rules-1"))
	n := s.cards["rules-1"]
	n.Text = "Refunds of €5\nor less, €50 a day."
	s.cards["rules-1"] = n
	if r := figuresUnsaid(s); !r.Grade.Pass {
		t.Errorf("€5 at a line's end still said: %+v", r.Grade)
	}
}

func invoicesSaid(profile string, disclose ...string) said {
	result := component{Type: "result", ID: "done"}
	result.Taf = &struct {
		Agent        string `json:"agent"`
		Name         string `json:"name"`
		Job          string `json:"job"`
		Draft        string `json:"draft"`
		Improves     string `json:"improves"`
		Note         string `json:"note"`
		Notes        string `json:"notes"`
		Kickoff      string `json:"kickoff"`
		Instructions string `json:"instructions"`
		Disclose     []struct {
			Tool   string   `json:"tool"`
			Fields []string `json:"fields"`
		} `json:"disclose"`
	}{Agent: "taf-invoices"}
	for _, d := range disclose {
		result.Taf.Disclose = append(result.Taf.Disclose, struct {
			Tool   string   `json:"tool"`
			Fields []string `json:"fields"`
		}{d, []string{"memo"}})
	}
	return said{
		cards:     map[string]component{"done": result},
		order:     []string{"done"},
		connected: []grantTool{{Tool: "create_draft_invoice", Name: "create_draft_invoice_q7"}},
		profiles:  []json.RawMessage{json.RawMessage(profile)},
	}
}

func TestADraftThatStaysInsideSaysSo(t *testing.T) {
	inside := `{"operations":[{"tool":"list_invoices","effect":"none"},{"tool":"create_draft_invoice","effect":"none_until_approved","resource":{"arg":"customer","noun":"invoice","outside":false}}]}`
	if r := gradeDraftStaysInside(invoicesSaid(inside)); !r.Grade.Pass || r.Name != draftStaysInside {
		t.Fatalf("outside: false, not disclosed: %+v", r)
	}
	for name, c := range map[string]struct {
		s    said
		want string
	}{
		"unsaid": {invoicesSaid(`{"operations":[{"tool":"create_draft_invoice","effect":"none_until_approved","resource":{"arg":"customer","noun":"invoice"}}]}`),
			"doesn't say outside: false"},
		"no resource": {invoicesSaid(`{"operations":[{"tool":"create_draft_invoice","effect":"none_until_approved"}]}`), "doesn't say outside: false"},
		"outside":     {invoicesSaid(strings.Replace(inside, `"outside":false`, `"outside":true`, 1)), "says outside: true"},
		"disclosed":   {invoicesSaid(inside, "create_draft_invoice_q7"), "is in the taf's disclose"},
	} {
		r := gradeDraftStaysInside(c.s)
		if r.Grade.Pass || r.Grade.Skipped || !strings.Contains(r.Grade.Why, c.want) {
			t.Errorf("%s: %+v, want a failure saying %q", name, r.Grade, c.want)
		}
	}
	// No profile with a draft (a key request's tools carry none): skipped, saying why,
	// and the reason shows in CI.
	for name, s := range map[string]said{
		"no draft":   invoicesSaid(`{"operations":[{"tool":"list_invoices","effect":"none"}]}`),
		"no profile": invoicesSaid(`not json`),
		"key path":   invoicesSaid(`null`),
	} {
		r := gradeDraftStaysInside(s)
		if !r.Grade.Skipped || r.Grade.Pass || !strings.Contains(r.Grade.Why, "a key request carries no profile, so tafs cloud checks no disclosure there") {
			t.Errorf("%s: %+v, want a skip naming the key path", name, r.Grade)
		}
	}
}

// Both gates' reasons name tools, rule fields and card ids, never the chat, so a
// public CI log shows them (Eval 16's draft fail showed no reason).
func TestTheNewGatesReasonsShowInCI(t *testing.T) {
	for _, name := range []string{rulesAsData, draftStaysInside} {
		if !counted[name] {
			t.Errorf("%q's reason is hidden from the CI log", name)
		}
	}
}
