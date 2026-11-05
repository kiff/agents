package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kiff/agents/eval"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/session"
)

// verdicts is a judge model that passes or fails each rule by a word in it,
// and keeps what it was asked.
type verdicts struct {
	mu    sync.Mutex
	fail  []string // a rule containing one of these fails
	asked []string
}

func (v *verdicts) Turn(_ context.Context, req model.Request, _ func(string)) (model.Reply, error) {
	prompt := req.Messages[0].Blocks[0].Text
	v.mu.Lock()
	v.asked = append(v.asked, prompt)
	v.mu.Unlock()
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
	s.messages = []string{"¿Con qué proveedor cobras?", strings.Repeat("palabra ", 150)}
	c := component{Type: "confirm", ID: "modo", Title: "Modo de Stripe", Text: "¿Prueba o real?", Note: "Puedes cambiarlo luego."}
	c.Options = append(c.Options, option{Value: "test", Label: "Prueba"})
	s.card(c)
	s.card(c) // a card shown again is one card

	// Length alone fails nothing: concise is the judge's (kiff/agents#185).
	v := &verdicts{fail: []string{"asks the owner to give", "Is each numbered message concise?"}}
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
	// Each check asks the judge Votes times (kiff/agents#209): read each rule once, in order.
	if len(v.asked)%eval.Votes != 0 {
		t.Fatalf("%d questions to the judge, not %d per check", len(v.asked), eval.Votes)
	}
	v.asked = slices.Compact(v.asked)
	// The language check reads Setup's cards too: their title, text and labels.
	if !strings.Contains(v.asked[0], "Spanish") || !strings.Contains(v.asked[0], "Modo de Stripe") || !strings.Contains(v.asked[0], "Prueba") ||
		!strings.Contains(v.asked[0], "Puedes cambiarlo luego.") {
		t.Fatalf("the language check read %q", v.asked[0])
	}
	// So does the id check: a confirm can ask for an id too.
	// The concise check is asked of the judge, about each message (kiff/agents#185).
	if !strings.Contains(v.asked[1], "fails only for one of these") || !strings.Contains(v.asked[1], "¿Con qué proveedor cobras?") {
		t.Fatalf("the concise check read %q", v.asked[1])
	}
	if !strings.Contains(v.asked[2], "asks the owner to give") || !strings.Contains(v.asked[2], "Modo de Stripe") {
		t.Fatalf("the id check read %q", v.asked[2])
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
// by name, that says its currency.
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

// A vendor with no test or live (the desk) has nothing for Setup to ask.
func TestNoModeToAskOfTheDesk(t *testing.T) {
	var s said
	s.messages = []string{"Next, approve the connection to your help desk and give its key on your phone."}
	j := &eval.Judge{Model: &verdicts{}, ModelID: "judge"}
	for _, m := range []struct {
		modes
		pass bool
	}{{modes{}, false}, {modes{none: true}, true}} {
		checks, err := gradeSetup(context.Background(), j, owners["en"], s, "taf-desk-x7k3", m.modes)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range checks {
			if c.Name == "asks test or live" && c.Pass != m.pass {
				t.Errorf("none %v: asks test or live pass %v, want %v", m.none, c.Pass, m.pass)
			}
		}
	}
}

// A question's options are the owner's choice, not Setup's claim: the mode
// question offering "Modo real" states no mode, whether in the whole chat or
// before test was reported; "Tu cuenta está en modo real" in a message, or
// in a card's own text, still does (the opus-low Eval, 5 Nov).
func TestAModeOfferedAsAnOptionIsNoClaim(t *testing.T) {
	ask := component{Type: "questions", ID: "modo"}
	ask.Questions = []question{{ID: "modo", Text: "¿En qué modo empieza?",
		Options: []option{{Value: "test", Label: "Modo de prueba", Description: "Recomendado para empezar"}, {Value: "live", Label: "Modo real"}}}}
	confirm := component{Type: "confirm", ID: "mode", Title: "Test or live?", Yes: "Live mode", No: "Test mode"}
	confirm.Options = []option{{Value: "live", Label: "Live mode (real money)"}}
	s := said{cards: map[string]component{"modo": ask, "mode": confirm}, order: []string{"modo", "mode"}}
	if c := claimsMode(s.statements(), true); c != "" {
		t.Fatalf("a mode offered as an option read as a claim: %q", c)
	}
	// The options are still in the texts other checks read (the language, no ids).
	if !slices.Contains(s.texts(), "Modo real") {
		t.Fatal("the options left the texts the other checks read")
	}
	// Said, it is a claim: in a message, or as a card's own text.
	s.messages = []string{"Tu cuenta está en modo real."}
	if c := claimsMode(s.statements(), true); c == "" {
		t.Fatal("the account's mode, stated in a message, wasn't a claim")
	}
	s.messages = nil
	said := component{Type: "result", ID: "done", Text: "Reembolsos funciona en modo real."}
	s.cards["done"], s.order = said, append(s.order, "done")
	if c := claimsMode(s.statements(), true); c == "" {
		t.Fatal("a card's own text stating the mode wasn't a claim")
	}

	// Before test was reported: the same, from the session's events.
	body, _ := json.Marshal(ask)
	before, _ := beforeTestReport([]session.Event{{Run: "r1", Kind: session.Component, Body: body}}, "r1", false)
	if c := claimsMode(before, false); c != "" {
		t.Fatalf("the mode question, before test was reported, read as a claim: %q (%v)", c, before)
	}
	if !slices.Contains(before, "¿En qué modo empieza?") {
		t.Fatalf("the question's own text left what's read before test: %v", before)
	}
}

// A turn's text its tool calls follow is a line, not a message: the owner
// sees it on the live line. Text that ends a turn, another run's and a blank
// one are what they were.
func TestPreToolTextIsALine(t *testing.T) {
	ev := func(run string, kind session.EventKind, body any) session.Event {
		b, _ := json.Marshal(body)
		return session.Event{Run: run, Kind: kind, Body: b}
	}
	text := func(s string) session.Event { return ev("r1", session.AgentMessage, map[string]string{"text": s}) }
	call := ev("r1", session.ToolCall, map[string]string{"call_id": "c1", "tool": "planque"})
	events := []session.Event{
		text("Miro las últimas ejecuciones de Reembolsos…"), ev("r1", session.Usage, map[string]int{"ms": 1}), call,
		ev("r1", session.ToolResult, map[string]string{"call_id": "c1"}),
		text("   "), call,
		ev("r2", session.AgentMessage, map[string]string{"text": "Another run's."}), call,
		text("Reembolsos está lista. Falta que apruebes la conexión."),
	}
	messages, lines := splitLines(events, "r1")
	if !slices.Equal(messages, []string{"Reembolsos está lista. Falta que apruebes la conexión."}) ||
		!slices.Equal(lines, []string{"Miro las últimas ejecuciones de Reembolsos…"}) {
		t.Fatalf("messages %q, lines %q", messages, lines)
	}
}

// A text its calls follow, that the run never writes after, is the run's
// message: "Falta que apruebes la conexión…" and the card it waits on (the
// confirming Eval, 6 Nov). A text the run writes after makes the earlier one
// a line, whatever came between.
func TestATextTheRunWaitsOnIsAMessage(t *testing.T) {
	ev := func(kind session.EventKind, body any) session.Event {
		b, _ := json.Marshal(body)
		return session.Event{Run: "r1", Kind: kind, Body: b}
	}
	text := func(s string) session.Event { return ev(session.AgentMessage, map[string]string{"text": s}) }
	show := ev(session.ToolCall, map[string]string{"call_id": "c1", "tool": "show"})
	messages, lines := splitLines([]session.Event{text("Falta que apruebes la conexión con tu proveedor de pagos."), show}, "r1")
	if !slices.Equal(messages, []string{"Falta que apruebes la conexión con tu proveedor de pagos."}) || lines != nil {
		t.Fatalf("waited on: messages %q, lines %q", messages, lines)
	}
	messages, lines = splitLines([]session.Event{
		text("Miro la cuenta…"), show, ev(session.ToolResult, map[string]string{"call_id": "c1"}),
		text("Reviso los límites…"), show, ev(session.Component, map[string]string{"type": "steps"}),
		text("Falta que apruebes la conexión."), show,
	}, "r1")
	if !slices.Equal(lines, []string{"Miro la cuenta…", "Reviso los límites…"}) || !slices.Equal(messages, []string{"Falta que apruebes la conexión."}) {
		t.Fatalf("messages %q, lines %q", messages, lines)
	}
}

// Each line is one short line, as Setup's rule asks: none passes; a line
// break or one past MaxLine characters fails, named.
func TestLinesAreOneShortLine(t *testing.T) {
	if g := shortLines(nil); !g.Pass {
		t.Fatalf("no lines: %+v", g)
	}
	if g := shortLines([]string{"Miro las últimas ejecuciones de Reembolsos…", strings.Repeat("ñ", MaxLine)}); !g.Pass {
		t.Fatalf("short lines: %+v", g)
	}
	g := shortLines([]string{"Reviso.", "Miro esto.\nY luego aquello.", strings.Repeat("a", MaxLine+1)})
	if g.Pass || !strings.Contains(g.Why, "line 2") || !strings.Contains(g.Why, "line 3") || strings.Contains(g.Why, "line 1") {
		t.Fatalf("long lines: %+v", g)
	}
}

// Lines are Setup's words, read for its language, ids, planque and a mode,
// never judged as messages.
func TestLinesAreReadButNotJudgedAsMessages(t *testing.T) {
	s := said{messages: []string{"Listo."}, lines: []string{"Miro la cuenta…"}}
	if got := s.texts(); !slices.Contains(got, "Miro la cuenta…") {
		t.Fatalf("texts %q", got)
	}
	if got := s.statements(); !slices.Contains(got, "Miro la cuenta…") {
		t.Fatalf("statements %q", got)
	}
}
