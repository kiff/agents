package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/kiff/agents/eval"
	"github.com/kiff/agents/model"
)

// verdicts is a judge model that passes or fails each rule by a word in it,
// and keeps what it was asked.
type verdicts struct {
	fail  []string // a rule containing one of these fails
	asked []string
}

func (v *verdicts) Turn(_ context.Context, req model.Request, _ func(string)) (model.Reply, error) {
	prompt := req.Messages[0].Blocks[0].Text
	v.asked = append(v.asked, prompt)
	pass := "true"
	for _, f := range v.fail {
		if strings.Contains(prompt, f) {
			pass = "false"
		}
	}
	return model.Reply{Blocks: []model.Block{{Kind: model.Text, Text: `{"pass": ` + pass + `, "why": "judged"}`}}}, nil
}

func TestTheSetupChatIsGraded(t *testing.T) {
	var s said
	s.messages = []string{"¿Con qué proveedor cobras?", strings.Repeat("palabra ", maxWords+1)}
	c := component{Type: "confirm", ID: "modo", Title: "Modo de Stripe", Text: "¿Prueba o real?", Note: "Puedes cambiarlo luego."}
	c.Options = append(c.Options, option{Value: "test", Label: "Prueba"})
	s.card(c)
	s.card(c) // a card shown again is one card

	v := &verdicts{fail: []string{"asks the owner to give"}}
	checks, err := gradeSetup(context.Background(), &eval.Judge{Model: v, ModelID: "judge", Price: model.Price{Input: 1, Output: 1}},
		owners["es"], s, "taf-reembolsos-x7k3", modes{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range checks {
		got[c.Name] = c.Pass
	}
	want := map[string]bool{"in the owner's language": true, "concise": false, "asks for no id": false,
		"names the agent from the taf's name": true, "offers choices to tap": true}
	for name, pass := range want {
		if p, ok := got[name]; !ok || p != pass {
			t.Errorf("%s: pass %v (present %v), want %v", name, p, ok, pass)
		}
	}
	if len(s.order) != 1 {
		t.Fatalf("cards %v", s.order)
	}
	// The language check reads Setup's cards too: their title, text and labels.
	if !strings.Contains(v.asked[0], "Spanish") || !strings.Contains(v.asked[0], "Modo de Stripe") || !strings.Contains(v.asked[0], "Prueba") ||
		!strings.Contains(v.asked[0], "Puedes cambiarlo luego.") {
		t.Fatalf("the language check read %q", v.asked[0])
	}
	// So does the id check: a confirm can ask for an id too.
	if !strings.Contains(v.asked[1], "asks the owner to give") || !strings.Contains(v.asked[1], "Modo de Stripe") {
		t.Fatalf("the id check read %q", v.asked[1])
	}
	if !counted["names no planque"] || counted["concise"] || counted["asks for no id"] {
		t.Fatal("only the checks graded without the judge print their reasons in CI")
	}

	// An agent id that doesn't carry the taf's name fails the naming check.
	checks, _ = gradeSetup(context.Background(), &eval.Judge{Model: &verdicts{}, ModelID: "judge"}, owners["es"], s, "taf-x7k3", modes{})
	for _, c := range checks {
		if c.Name == "names the agent from the taf's name" && c.Pass {
			t.Fatal("an agent id without the taf's name passed")
		}
	}
	// A write asked for as read-only, refused by planque, fails and says which (kiff/agents#139).
	refused := s
	refused.writesAsReads = []string{"payout_balance"}
	checks, _ = gradeSetup(context.Background(), &eval.Judge{Model: &verdicts{}, ModelID: "judge"}, owners["es"], refused, "taf-reembolsos-x7k3", modes{})
	for _, c := range checks {
		if c.Name == "marks no write read-only" && (c.Pass || !strings.Contains(c.Why, "payout_balance")) {
			t.Fatalf("a write asked for as read-only: %+v", c)
		}
	}
	for text, want := range map[string]string{
		// planque's notReadOnly, and as the planque tool returns it.
		"payout_balance isn't read-only at vendor.test: the vendor doesn't mark it so, so it can't be granted as a read":                      "payout_balance",
		"planque answered 400: create_refund isn't read-only at vendor.test: the vendor doesn't mark it so, so it can't be granted as a read": "create_refund",
		"vendor.test lists no tool named list_payouts": "",
	} {
		if got, _ := refusedRead(text); got != want {
			t.Errorf("refusedRead(%q) = %q, want %q", text, got, want)
		}
	}
	if slug("Reembolsos de Tienda") != "reembolsos-de-tienda" {
		t.Fatalf("slug %q", slug("Reembolsos de Tienda"))
	}
}

// The owner never meets planque, and a plan once shown keeps its steps
// (kiff/tafs#82). Both are counted, and say why without quoting the chat.
func TestNoPlanqueAndAStablePlan(t *testing.T) {
	step := func(label, status string) struct {
		Label  string `json:"label"`
		Status string `json:"status"`
	} {
		return struct {
			Label  string `json:"label"`
			Status string `json:"status"`
		}{label, status}
	}
	grade := func(s said) map[string]eval.CheckResult {
		checks, err := gradeSetup(context.Background(), &eval.Judge{Model: &verdicts{}, ModelID: "judge"}, owners["es"], s, "taf-reembolsos-x7k3", modes{})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]eval.CheckResult{}
		for _, c := range checks {
			out[c.Name] = c
		}
		return out
	}

	var ok said
	ok.messages = []string{"Conecto tu Stripe."}
	plan := component{Type: "steps", ID: "plan"}
	plan.Steps = append(plan.Steps, step("Conectar Stripe", "active"), step("Dar su Card", "pending"))
	ok.card(plan)
	plan.Steps = nil
	plan.Steps = append(plan.Steps, step("Conectar Stripe", "done"), step("Dar su Card", "active"))
	ok.card(plan) // only the statuses moved
	got := grade(ok)
	if !got["names no planque"].Pass || !got["keeps its plan"].Pass {
		t.Fatalf("a plan whose statuses moved: %+v", got)
	}

	var bad said
	bad.messages = []string{"Abre planque en la app."}
	plan = component{Type: "steps", ID: "plan"}
	plan.Steps = append(plan.Steps, step("Conectar Stripe", "active"), step("Dar su Card", "pending"))
	bad.card(plan)
	plan.Steps = nil
	plan.Steps = append(plan.Steps, step("Revisar herramientas", "active"), step("Dar su Card", "pending"), step("Poner a trabajar", "pending"))
	bad.card(plan)
	got = grade(bad)
	if c := got["names no planque"]; c.Pass || c.Why != "text 1 names planque" || !counted[c.Name] {
		t.Fatalf("planque named: %+v", c)
	}
	if c := got["keeps its plan"]; c.Pass || c.Why != "the plan 2 steps, then 3" || !counted[c.Name] || strings.Contains(c.Why, "Revisar") {
		t.Fatalf("a plan that changed: %+v", c)
	}
}

// Test or live is the owner's choice (kiff/tafs#89): the scripted owner taps
// test, Setup must have offered both, and nothing may be pinned to live.
func TestTestOrLiveIsTheOwnersChoice(t *testing.T) {
	opt := func(value, label string) option { return option{Value: value, Label: label} }
	es := component{Type: "confirm", ID: "modo"}
	es.Options = append(es.Options, opt("real", "Modo real (dinero real)"), opt("prueba", "Modo prueba (sin dinero real), recomendado"))
	if v, ok := modeChoice(es); !ok || v != "prueba" {
		t.Fatalf("Spanish choice: %q %v, want prueba", v, ok)
	}
	en := component{Type: "confirm", ID: "mode"}
	en.Options = append(en.Options, opt("test", "Test mode (no real money), recommended to start"), opt("live", "Live mode (real money)"))
	if v, ok := modeChoice(en); !ok || v != "test" {
		t.Fatalf("English choice: %q %v, want test", v, ok)
	}
	other := component{Type: "confirm", ID: "ok"}
	other.Options = append(other.Options, opt("yes", "Yes"), opt("no", "Change them"))
	if _, ok := modeChoice(other); ok {
		t.Fatal("a yes/no confirm read as test or live")
	}

	for _, c := range []struct {
		fixed map[string]any
		live  bool
	}{
		{map[string]any{"livemode": true}, true},
		{map[string]any{"livemode": "true"}, true},
		{map[string]any{"livemode": false, "account": "acct_1"}, false},
		{map[string]any{"mode": "live"}, true},
		{map[string]any{"environment": "test"}, false},
		{nil, false},
	} {
		if got := pinnedLive(c.fixed); got != c.live {
			t.Errorf("pinnedLive(%v) = %v", c.fixed, got)
		}
	}

	grade := func(cards []component, m modes) map[string]eval.CheckResult {
		var s said
		for _, c := range cards {
			s.card(c)
		}
		checks, err := gradeSetup(context.Background(), &eval.Judge{Model: &verdicts{}, ModelID: "judge"}, owners["en"], s, "taf-refunds-x7k3", m)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]eval.CheckResult{}
		for _, c := range checks {
			out[c.Name] = c
		}
		return out
	}
	got := grade([]component{en}, modes{})
	if !got["asks test or live"].Pass || !got["pins the mode the owner chose"].Pass {
		t.Fatalf("asked, pinned test: %+v", got)
	}
	got = grade([]component{other}, modes{live: []string{"create_refund"}})
	if got["asks test or live"].Pass || got["pins the mode the owner chose"].Pass {
		t.Fatalf("never asked, pinned live: %+v", got)
	}
	if !strings.Contains(got["pins the mode the owner chose"].Why, "create_refund") {
		t.Fatalf("the reason names no tool: %q", got["pins the mode the owner chose"].Why)
	}
	// Pinned test without asking is a guess (nothing is read from the
	// account before its tools are approved, kiff/planque#23): it had to ask.
	if got = grade([]component{other}, modes{test: true}); got["asks test or live"].Pass {
		t.Fatalf("pinned test, never asked: %+v", got)
	}
	for fixed, want := range map[string]string{`{"mode":"test"}`: "test", `{"livemode":false}`: "test", `{"environment":"sandbox"}`: "test", `{"mode":"live"}`: "live", `{"account":"a"}`: ""} {
		var f map[string]any
		_ = json.Unmarshal([]byte(fixed), &f)
		if got := pinnedMode(f); got != want {
			t.Errorf("pinnedMode(%s) = %q, want %q", fixed, got, want)
		}
	}
}

// A tool that moves money names its resource and a read of the same grant,
// by name, that says its currency (kiff/kiff-cloud#1299).
func TestUncheckedMoney(t *testing.T) {
	lookup := &struct {
		Tool string `json:"tool"`
	}{Tool: "get_charge"}
	read := grantTool{Tool: "stripe_api_read", Name: "get_charge", ReadOnly: true}
	refund := grantTool{Tool: "stripe_api_write", Name: "refund_payment", AmountArg: "amount", ResourceArg: "charge", UnitLookup: lookup}
	if got := uncheckedMoney([]grantTool{refund, read}); len(got) != 0 {
		t.Fatalf("checked refund: %v", got)
	}
	noResource, noLookup, noRead := refund, refund, refund
	noResource.ResourceArg, noLookup.UnitLookup = "", nil
	for name, tools := range map[string][]grantTool{"no resource": {noResource, read}, "no lookup": {noLookup, read}, "no such read": {noRead}} {
		if got := uncheckedMoney(tools); len(got) != 1 {
			t.Errorf("%s: %v", name, got)
		}
	}
	if got := uncheckedMoney([]grantTool{read}); len(got) != 0 {
		t.Fatalf("a read alone: %v", got)
	}
}

// A questions card (kiff/tafs#138 item 57) is answered in one message, as the
// app sends it: test for a test-or-live question, the owner's facts typed for
// the first other one; and its test-or-live question counts as asking.
// Questions asked one confirm after another are judged; one card, or a
// single confirm, needs no judge (kiff/tafs#138, item 57).
func TestQuestionsAskedTogetherAreGraded(t *testing.T) {
	mode := component{Type: "confirm", ID: "modo", Text: "¿Prueba o real?", Options: []option{{Value: "test", Label: "Prueba"}, {Value: "live", Label: "Real"}}}
	limit := component{Type: "confirm", ID: "limite", Text: "¿Cuánto al día?", Options: []option{{Value: "100", Label: "100 €"}, {Value: "250", Label: "250 €"}}}
	var apart said
	apart.card(mode)
	apart.card(limit)
	v := &verdicts{fail: []string{"asked together"}}
	checks, err := gradeSetup(context.Background(), &eval.Judge{Model: v, ModelID: "judge"}, owners["es"], apart, "taf-reembolsos-x7k3", modes{})
	if err != nil {
		t.Fatal(err)
	}
	if pass(checks, "asks its questions together") {
		t.Fatal("two confirms one after another passed")
	}
	last := v.asked[len(v.asked)-1]
	if !strings.Contains(last, "1. confirm: ¿Prueba o real? [Prueba | Real]") || !strings.Contains(last, "2. confirm: ¿Cuánto al día? [100 € | 250 €]") {
		t.Fatalf("the judge read %q", last)
	}

	together := component{Type: "questions", ID: "basics", Questions: []question{
		{ID: "modo", Text: "¿Prueba o real?", Options: mode.Options},
		{ID: "limite", Text: "¿Cuánto al día?", Options: limit.Options},
	}}
	for name, cards := range map[string][]component{"one card": {together}, "one confirm": {mode}} {
		var s said
		for _, c := range cards {
			s.card(c)
		}
		v := &verdicts{fail: []string{"asked together"}}
		checks, err := gradeSetup(context.Background(), &eval.Judge{Model: v, ModelID: "judge"}, owners["es"], s, "taf-reembolsos-x7k3", modes{})
		if err != nil {
			t.Fatal(err)
		}
		if !pass(checks, "asks its questions together") {
			t.Errorf("%s failed", name)
		}
		for _, a := range v.asked {
			if strings.Contains(a, "asked together") {
				t.Errorf("%s asked the judge", name)
			}
		}
	}
}

func pass(checks []eval.CheckResult, name string) bool {
	for _, c := range checks {
		if c.Name == name {
			return c.Pass
		}
	}
	return false
}

func TestAQuestionsCardIsAnsweredAndGraded(t *testing.T) {
	c := component{Type: "questions", ID: "basics", Questions: []question{
		{ID: "mode", Text: "¿Prueba o real?", Options: []option{{Value: "real", Label: "Dinero real"}, {Value: "prueba", Label: "Modo prueba"}}},
		{ID: "limit", Text: "¿Cuánto al día?", Options: []option{{Value: "10000", Label: "100 €"}, {Value: "25000", Label: "250 €"}}},
	}}
	if got, want := answerQuestions(c, "50 € al día"), `[questions basics] {"limit":{"own":"50 € al día"},"mode":{"value":"prueba"}}`; got != want {
		t.Fatalf("answer %s, want %s", got, want)
	}
	if got, want := answerQuestions(c, ""), `[questions basics] {"limit":{"value":"10000"},"mode":{"value":"prueba"}}`; got != want {
		t.Fatalf("answer %s, want %s", got, want)
	}
	if v, ok := modeChoice(c); !ok || v != "prueba" {
		t.Fatalf("mode choice %q %v", v, ok)
	}
	var s said
	s.card(c)
	if n := len(confirms(s)); n != 2 {
		t.Fatalf("choices to tap = %d, want 2", n)
	}
	if texts := s.texts(); !slices.Contains(texts, "¿Cuánto al día?") || !slices.Contains(texts, "250 €") {
		t.Fatalf("texts %v", texts)
	}
}
