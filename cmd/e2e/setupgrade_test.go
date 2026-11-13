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

// A question's options are the owner's choice, not Setup's statement: the
// mode question offering "Modo real" leaves its options out of what the judged
// mode check reads, whether in the whole chat or before test was reported,
// while the language and id checks still read them (the opus-low Eval, 5 Nov).
func TestAModeOfferedAsAnOptionIsNoClaim(t *testing.T) {
	ask := component{Type: "questions", ID: "modo"}
	ask.Questions = []question{{ID: "modo", Text: "¿En qué modo empieza?",
		Options: []option{{Value: "test", Label: "Modo de prueba", Description: "Recomendado para empezar"}, {Value: "live", Label: "Modo real"}}}}
	confirm := component{Type: "confirm", ID: "mode", Title: "Test or live?", Yes: "Live mode", No: "Test mode"}
	confirm.Options = []option{{Value: "live", Label: "Live mode (real money)"}}
	s := said{cards: map[string]component{"modo": ask, "mode": confirm}, order: []string{"modo", "mode"}}
	if slices.Contains(s.statements(), "Modo real") || slices.Contains(s.statements(), "Live mode (real money)") {
		t.Fatalf("an option read as Setup's statement: %q", s.statements())
	}
	// The options are still in the texts other checks read (the language, no ids).
	if !slices.Contains(s.texts(), "Modo real") {
		t.Fatal("the options left the texts the other checks read")
	}
	// Before test was reported: the question's own text, never its options.
	body, _ := json.Marshal(ask)
	before, _ := beforeTestReport([]session.Event{{Run: "r1", Kind: session.Component, Body: body}}, "r1", false)
	if !slices.Contains(before, "¿En qué modo empieza?") || slices.Contains(before, "Modo real") {
		t.Fatalf("what's read before test: %v", before)
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
	messages, lines, _ := splitLines(events, "r1")
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
	messages, lines, _ := splitLines([]session.Event{text("Falta que apruebes la conexión con tu proveedor de pagos."), show}, "r1")
	if !slices.Equal(messages, []string{"Falta que apruebes la conexión con tu proveedor de pagos."}) || lines != nil {
		t.Fatalf("waited on: messages %q, lines %q", messages, lines)
	}
	messages, lines, _ = splitLines([]session.Event{
		text("Miro la cuenta…"), show, ev(session.ToolResult, map[string]string{"call_id": "c1"}),
		text("Reviso los límites…"), show, ev(session.Component, map[string]string{"type": "steps"}),
		text("Falta que apruebes la conexión."), show,
	}, "r1")
	if !slices.Equal(lines, []string{"Miro la cuenta…"}) || !slices.Equal(messages, []string{"Reviso los límites…", "Falta que apruebes la conexión."}) {
		t.Fatalf("a card before the next text: messages %q, lines %q", messages, lines)
	}
	// The run waits on the owner, then goes on and writes again: the text it waited on stays its message.
	resumed := ev(session.Usage, map[string]any{"timing": map[string]any{"ms": 900, "resumed": true}})
	messages, lines, _ = splitLines([]session.Event{
		text("Falta que lo apruebes."), ev(session.ToolCall, map[string]string{"call_id": "c2", "tool": "planque"}),
		resumed, text("Conectado. Ya puede reembolsar."),
	}, "r1")
	if lines != nil || !slices.Equal(messages, []string{"Falta que lo apruebes.", "Conectado. Ya puede reembolsar."}) {
		t.Fatalf("after a pause: messages %q, lines %q", messages, lines)
	}
	// The owner answers a card (the connection's approval), or writes, before the run writes
	// again, with no turn marked resumed between: it waited on them, so the text stays its
	// message (Eval 42177781403: "Falta que apruebes la conexión con tu proveedor de pagos y que
	// inicies sesión en su página.", then "Falta que elijas el modo…").
	waited := "Falta que apruebes la conexión con tu proveedor de pagos y que inicies sesión en su página."
	for _, owner := range []session.Event{
		ev(session.Answered, map[string]string{"id": "conectar", "type": "approval"}),
		ev(session.OwnerMessage, map[string]string{"text": "Listo, aprobado."}),
	} {
		messages, lines, _ = splitLines([]session.Event{
			text(waited), ev(session.ToolCall, map[string]string{"call_id": "c3", "tool": "planque"}),
			ev(session.ToolResult, map[string]string{"call_id": "c3"}), owner,
			ev(session.Usage, map[string]any{"timing": map[string]any{"ms": 700}}),
			text("Falta que elijas el modo y si puedo leer la lista de tus cuentas."),
		}, "r1")
		if lines != nil || !slices.Equal(messages, []string{waited, "Falta que elijas el modo y si puedo leer la lista de tus cuentas."}) {
			t.Fatalf("after the owner's %s: messages %q, lines %q", owner.Kind, messages, lines)
		}
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

// A run read again after it went on counts only what it wrote since: the
// confirming Eval listed "Falta que apruebes la conexión…" four times.
func TestARunReadAgainCountsOnce(t *testing.T) {
	tt := &test{}
	tt.take("r1", []string{"Falta que apruebes la conexión."}, []string{"Miro la cuenta…"}, []string{"Falta que apruebes la conexión."})
	tt.take("r1", []string{"Falta que apruebes la conexión.", "Conectado."}, []string{"Miro la cuenta…"}, []string{"Falta que apruebes la conexión."})
	tt.take("r2", []string{"Falta que apruebes la conexión."}, nil, nil)
	if !slices.Equal(tt.said.messages, []string{"Falta que apruebes la conexión.", "Conectado.", "Falta que apruebes la conexión."}) ||
		!slices.Equal(tt.said.lines, []string{"Miro la cuenta…"}) || !slices.Equal(tt.said.intros, []string{"Falta que apruebes la conexión."}) {
		t.Fatalf("messages %q, lines %q, intros %q", tt.said.messages, tt.said.lines, tt.said.intros)
	}

	// Read first as a message, then, once the run wrote again, as a line: it counts once, as the
	// message it was taken as, and no line check reads it (Eval 42177781403, agentgabo). A text
	// the run wrote twice still counts twice.
	waited := "Falta que apruebes la conexión con tu proveedor de pagos y que inicies sesión en su página."
	tt = &test{}
	tt.take("r1", []string{waited}, nil, nil)
	tt.take("r1", []string{"Falta que elijas el modo."}, []string{waited}, nil)
	tt.take("r1", []string{"Falta que elijas el modo.", "Listo.", "Listo."}, []string{waited}, nil)
	if !slices.Equal(tt.said.messages, []string{waited, "Falta que elijas el modo.", "Listo.", "Listo."}) || tt.said.lines != nil {
		t.Fatalf("a message read again as a line: messages %q, lines %q", tt.said.messages, tt.said.lines)
	}
	if g := shortLines(tt.said.lines); !g.Pass {
		t.Fatalf("the 91 characters failed the line check: %+v", g)
	}
}

// The one short line Setup writes before it works reaches the chat as text when
// a card, a pause or the end comes before it writes again. Where it sits decides
// it, however it ends, so the concise judge never reads it as a message's
// opening (Eval 42136777522's "…" and Eval 42141761407's ".", the desk; qa-kiff).
// A short message that ends a turn with no call after it stays judged.
func TestAProgressLineIsNoMessageForTheConciseJudge(t *testing.T) {
	ev := func(kind session.EventKind, body any) session.Event {
		b, _ := json.Marshal(body)
		return session.Event{Run: "r1", Kind: kind, Body: b}
	}
	text := func(s string) session.Event { return ev(session.AgentMessage, map[string]string{"text": s}) }
	call := ev(session.ToolCall, map[string]string{"call_id": "c1", "tool": "planque"})
	result := ev(session.ToolResult, map[string]string{"call_id": "c1"})
	card := ev(session.Component, map[string]string{"type": "questions"})
	for _, line := range []string{
		"I'm checking which apps your account already connects.",  // Eval 42141761407, the desk
		"I'm checking which tools your account already connects…", // Eval 42136777522, the desk
		"Miro qué apps conecta ya tu cuenta",                      // no closing mark at all
	} {
		messages, _, intros := splitLines([]session.Event{text(line), call, result, card}, "r1")
		if !slices.Equal(messages, []string{line}) || !slices.Equal(intros, []string{line}) {
			t.Fatalf("%q: messages %q, intros %q", line, messages, intros)
		}
		if judged := withoutIntros(messages, intros); len(judged) != 0 {
			t.Fatalf("%q reached the concise judge", line)
		}
	}
	// A short closing message, with no call after it, is judged; so is a long text before a card.
	closing := "Listo. Reembolsos está activo."
	long := "Revisé tus cuentas, los cobros de la última semana, las reglas de reembolso y lo que ya conecta tu cuenta."
	messages, _, intros := splitLines([]session.Event{text(long), call, result, card, text(closing)}, "r1")
	if judged := withoutIntros(messages, intros); !slices.Equal(judged, []string{long, closing}) {
		t.Fatalf("judged %q (messages %q, intros %q)", judged, messages, intros)
	}
	// An intro is taken out once: the same words said again as a message are judged.
	if judged := withoutIntros([]string{"Miro la cuenta.", "Miro la cuenta."}, []string{"Miro la cuenta."}); len(judged) != 1 {
		t.Fatalf("judged %q", judged)
	}
}

// The mode is a fact in the grant: pinned only to what the owner chose and
// the connection reports, or not at all (the owner, 6 Nov 2026: results gate).
func TestThePinIsTheModeTheOwnerChose(t *testing.T) {
	for _, c := range []struct {
		name  string
		chose string
		m     modes
		pass  bool
		why   string
	}{
		{"test, as chosen and reported", "test", modes{test: true, allTest: true}, true, ""},
		{"nothing pinned", "", modes{}, true, ""},
		{"test chosen, nothing pinned", "test", modes{}, false, "not every tool that acts is pinned to it"},
		{"test chosen, one acting tool unpinned", "test", modes{test: true, allTest: false}, false, "not every tool that acts is pinned to it"},
		{"test, with no choice made", "", modes{test: true}, false, "the owner chose no mode"},
		{"live, though test was chosen", "test", modes{live: []string{"stripe_refund"}}, false, "pinned to live mode, though the owner chose test"},
		{"a pin on the desk, which has no mode", "test", modes{none: true, test: true}, false, "a vendor that has none"},
		{"the desk, nothing pinned", "", modes{none: true}, true, ""},
	} {
		g := pinned(c.chose, c.m)
		if g.Pass != c.pass || !strings.Contains(g.Why, c.why) {
			t.Errorf("%s: %+v", c.name, g)
		}
	}
}

// The judged mode check is told what the connection reported and what the
// owner chose, and which texts came before the report: without them a judge
// can't tell a claim from a report (qa-kiff). It's a gate; the wording checks
// around it are advice.
func TestTheModeJudgeIsToldTheFacts(t *testing.T) {
	ask := func(chose string, m modes, s said) string {
		t.Helper()
		v := &verdicts{}
		s.chose = chose
		if _, err := gradeSetup(context.Background(), &eval.Judge{Model: v, ModelID: "judge"}, owners["en"], s, "taf-refunds-x7k3", m); err != nil {
			t.Fatal(err)
		}
		for _, p := range v.asked {
			if strings.Contains(p, "states as fact which mode the owner's account itself is in") {
				return p
			}
		}
		t.Fatalf("the mode question wasn't asked: %d prompts", len(v.asked))
		return ""
	}
	early := "Your account is in test mode, so nothing real moves."
	s := said{messages: []string{early, "Every action is pinned to test mode, as you chose."}, beforeTest: []string{early}}
	p := ask("test", modes{test: true, allTest: true}, s)
	for _, want := range []string{"The connection reported that the owner's account is in test mode.", "The owner chose test mode for the calls.",
		"1. [before the report] " + early, "2. Every action is pinned to test mode, as you chose."} {
		if !strings.Contains(p, want) {
			t.Errorf("the question lacks %q:\n%s", want, p)
		}
	}
	// The Eval of agents#357's text 6, after the report and saying what it reported: no wording is
	// asked of it, so the question never asks it to name the connection.
	text6 := "Your account there is in test mode."
	p = ask("test", modes{test: true, allTest: true}, said{messages: []string{early, text6}, beforeTest: []string{early}})
	for _, want := range []string{"2. " + text6, "states the mode the connection reported passes, however it's worded", "is marked [before the report]",
		"states another mode than the one the connection reported"} {
		if !strings.Contains(p, want) {
			t.Errorf("the question lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "[before the report] "+text6) || strings.Contains(p, "unless it says what the connection reported") {
		t.Errorf("the question still asks for the report's words, or marks text 6:\n%s", p)
	}
	p = ask("", modes{none: true}, said{messages: []string{"The desk runs in live mode."}})
	for _, want := range []string{"The vendor has no test or live mode at all", "The owner chose no mode."} {
		if !strings.Contains(p, want) {
			t.Errorf("the desk's question lacks %q:\n%s", want, p)
		}
	}

	// It gates, and the wording checks are advice.
	checks, err := gradeSetup(context.Background(), &eval.Judge{Model: &verdicts{fail: []string{"states as fact which mode", "Is each numbered message concise?"}}, ModelID: "judge"},
		owners["en"], said{messages: []string{early}}, "taf-refunds-x7k3", modes{})
	if err != nil {
		t.Fatal(err)
	}
	kind := map[string]bool{}
	for _, c := range checks {
		kind[c.Name] = c.Advisory
	}
	if kind["states the account's mode only as the connection reports it"] || !kind["concise"] || !kind["asks test or live"] || kind["pins the mode the owner chose"] {
		t.Fatalf("gate and advice: %v", kind)
	}
}

// Advice is reported and never fails the setup chat; a gate check that fails
// does (the owner, 6 Nov 2026: words and tone are never a gate).
func TestAdviceNeverFailsTheSetup(t *testing.T) {
	var log strings.Builder
	tt := &test{cfg: config{log: &log, judge: &eval.Judge{}}, out: &redacting{w: &log}}
	err := tt.reportSetup([]eval.CheckResult{
		{Name: "concise", Advisory: true, Grade: eval.Grade{Why: "wordy"}},
		{Name: "says what it's doing in one short line", Advisory: true, Grade: eval.Grade{Why: "line 1 (91 characters)"}},
		{Name: "pins the mode the owner chose", Grade: eval.Grade{Pass: true}},
	})
	if err != nil || !strings.Contains(log.String(), "  advice  concise") || !strings.Contains(log.String(), "setup  1/1 gate, 2 advice") {
		t.Fatalf("err %v, log %q", err, log.String())
	}
	log.Reset()
	err = tt.reportSetup([]eval.CheckResult{
		{Name: "concise", Advisory: true, Grade: eval.Grade{Why: "wordy"}},
		{Name: "pins the mode the owner chose", Grade: eval.Grade{Why: "pinned to live mode"}},
	})
	if err == nil || !strings.Contains(err.Error(), "pins the mode the owner chose") || strings.Contains(err.Error(), "concise") {
		t.Fatalf("a failed gate check: %v", err)
	}
}

// The Card KIFF holds is graded against the rules the owner agreed: the
// notices Setup showed, in order, and nothing when it showed none or KIFF holds
// no Card. A judge that fails it fails the gate (a from-zero test on a phone:
// agreed "1,000 reads a day each", signed "4,000 a day" for all).
func TestTheCardIsGradedAgainstTheAgreedRules(t *testing.T) {
	s := said{cards: map[string]component{
		"r1":   {Type: "notice", Title: "The rules", Text: "Reads only, up to 1,000 a day each."},
		"ask":  {Type: "confirm", Title: "Agree to these rules?"},
		"r2":   {Type: "notice", Title: "The rules, changed", Text: "Reads only: up to 1,000 a day for each of the 4 reads."},
		"ask2": {Type: "confirm", Title: "Agree to these rules?"},
		"done": {Type: "notice", Title: "Connected", Text: "Qonto is connected, read only."},
		"plan": {Type: "steps"},
	}, order: []string{"r1", "ask", "r2", "ask2", "done", "plan"}}
	// Only what the owner was asked to agree to: a notice with a confirm after it. "Connected" states no rule.
	if got := agreedRules(s); len(got) != 2 || !strings.Contains(got[0], "1,000 a day each") || !strings.HasPrefix(got[1], "The rules, changed") {
		t.Fatalf("the rules shown: %q", got)
	}
	cards := []map[string]any{{"domain": "gateway", "on_exceed": "owner", "grants": []map[string]any{{"action": "list_invoices"}}}}
	v := &verdicts{}
	r, err := gradeAgreed(context.Background(), &eval.Judge{Model: v, ModelID: "judge"}, s, cards)
	if err != nil || r.Name != signsWhatWasAgreed || !r.Grade.Pass || r.Advisory {
		t.Fatalf("a Card that keeps the rules: %+v %v", r, err)
	}
	// Words only the notices and the Cards have, never the rule's own: the judge is shown both, and nothing else counts as a rule.
	if asked := strings.Join(v.asked, "\n"); !strings.Contains(asked, "for each of the 4 reads") || !strings.Contains(asked, "list_invoices") || strings.Contains(asked, "Qonto is connected") {
		t.Fatal("the judge wasn't shown the rules and the Cards, or was shown a notice that states no rule")
	}
	// An amount's max is in the call's units: the judge is told so, so 150 EUR in the rules reads as 15000 on the Card.
	if !strings.Contains(agreedRule, "15000") || !strings.Contains(agreedRule, "150.00 EUR") {
		t.Fatal("the rule doesn't say how an amount's max reads")
	}
	// A judge that finds a merged limit fails the gate.
	r, _ = gradeAgreed(context.Background(), &eval.Judge{Model: &verdicts{fail: []string{"merged with another"}}, ModelID: "judge"}, s, cards)
	if r.Grade.Pass || r.Grade.Skipped || r.Advisory {
		t.Fatalf("a merged limit passed: %+v", r)
	}
	// Nothing to compare: skipped, never passed.
	for _, c := range []struct {
		s     said
		cards []map[string]any
	}{{said{}, cards}, {s, nil}} {
		r, _ = gradeAgreed(context.Background(), &eval.Judge{Model: &verdicts{}, ModelID: "judge"}, c.s, c.cards)
		if !r.Grade.Skipped || r.Grade.Pass {
			t.Fatalf("with nothing to compare: %+v", r)
		}
	}
}
