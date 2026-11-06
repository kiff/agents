package main

import (
	"encoding/json"
	"io"
	"testing"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool"
)

// A conversation opens with the skills tafs cloud would name for its entry
// point, as stored versions in the definition's order; a definition that names
// none of them (kiff/tafs#218's names, or none) sends none.
func TestStartSkillsFollowTafsCloudsTable(t *testing.T) {
	tt := &test{tenant: "t1", cfg: config{log: io.Discard},
		setupSkills:    []string{"cards-and-holds", "connect-an-app", "draft-the-card", "halting", "improve-a-taf", "name-the-tools", "set-up-a-taf", "the-dial", "the-tafs", "usage-and-plans"},
		setupSkillRefs: []string{"cards-and-holds@1", "connect-an-app@1", "draft-the-card@1", "halting@1", "improve-a-taf@1", "name-the-tools@2", "set-up-a-taf@1", "the-dial@1", "the-tafs@1", "usage-and-plans@1"}}
	want := map[string][]string{
		"free":    {"connect-an-app@1", "draft-the-card@1", "name-the-tools@2", "set-up-a-taf@1", "usage-and-plans@1"},
		"improve": {"connect-an-app@1", "draft-the-card@1", "improve-a-taf@1", "name-the-tools@2"},
		"other":   nil,
	}
	for entry, w := range want {
		got := tt.startSkills(entry)
		if len(got) != len(w) {
			t.Fatalf("%s: %v, want %v", entry, got, w)
		}
		for i := range w {
			if got[i] != w[i] {
				t.Fatalf("%s: %v, want %v", entry, got, w)
			}
		}
	}
	old := &test{tenant: "t1", cfg: config{log: io.Discard}, setupSkills: []string{"change-a-taf"}, setupSkillRefs: []string{"change-a-taf@1"}}
	if got := old.startSkills("free"); len(got) != 0 {
		t.Fatalf("an older definition sent %v", got)
	}
	if got := old.skillOf([]string{"improve-a-taf", "change-a-taf"}); got != "change-a-taf" {
		t.Fatalf("the question's skill by the older name: %q", got)
	}
	if got := tt.skillOf([]string{"cards-and-holds", "explain-a-hold"}); got != "cards-and-holds" {
		t.Fatalf("the question's skill by the new name: %q", got)
	}
}

// Setup states only the mode the connection reports: with test reported, a live
// mode or real money is a claim and test isn't; with none reported, any mode is.
func TestClaimsModeReadsWhatTheConnectionReports(t *testing.T) {
	test := []string{"Chosen: test mode. Up to 5 EUR a refund.", "Stripe in test mode, no real money.", "Reembolsos en modo de prueba, sin dinero real."}
	if c := claimsMode(test, true); c != "" {
		t.Fatalf("test mode, which the vendor reports, read as a claim: %q", c)
	}
	for _, claim := range []string{"Live mode (real money) from now on.", "Esto mueve dinero real.", "Refunds runs in live mode."} {
		if c := claimsMode([]string{"fine", claim}, true); c != claim {
			t.Fatalf("%q not read as a claim with test reported: %q", claim, c)
		}
	}
	for _, claim := range []string{"Desk in test mode.", "No real money moves here.", "Modo de prueba activado."} {
		if c := claimsMode([]string{claim}, false); c != claim {
			t.Fatalf("%q not read as a claim with no mode reported: %q", claim, c)
		}
	}
	if c := claimsMode([]string{"Up to 20 replies a day."}, false); c != "" {
		t.Fatalf("a text with no mode read as a claim: %q", c)
	}
}

// A test mode said before any tool result reported it is a claim; after a
// read that returned livemode false, it's what the connection reports
// (agentgabo on #319: the same sentence, before and after).
func TestModeCountsAsReportedOnlyAfterAToolSaidSo(t *testing.T) {
	ev := func(kind session.EventKind, body any) session.Event {
		b, _ := json.Marshal(body)
		return session.Event{Run: "r1", Kind: kind, Body: b}
	}
	say := ev(session.AgentMessage, agent.TextBody{Text: "Reembolsos en modo de prueba."})
	read := ev(session.ToolResult, agent.ToolResultBody{Tool: "read", Result: tool.Result{Text: `{"id":"ch_1","object":"charge","livemode": false}`}})
	accounts := ev(session.ToolResult, agent.ToolResultBody{Tool: "read", Result: tool.Result{Text: `{"accounts":[{"account":"acct_1","mode":"test"}]}`}})
	refused := ev(session.ToolResult, agent.ToolResultBody{Tool: "read", Result: tool.Result{Text: `"livemode": false`, IsError: true}})

	before, reported := beforeTestReport([]session.Event{say, read}, "r1", false)
	if len(before) != 1 || !reported {
		t.Fatalf("said before the read: %v %v", before, reported)
	}
	if c := claimsMode(before, false); c == "" {
		t.Fatal("a test mode said before any read wasn't a claim")
	}
	before, reported = beforeTestReport([]session.Event{read, say}, "r1", false)
	if len(before) != 0 || !reported {
		t.Fatalf("said after the read: %v %v", before, reported)
	}
	if _, r := beforeTestReport([]session.Event{accounts}, "r1", false); !r {
		t.Fatal("an accounts read with mode test didn't report it")
	}
	if b, r := beforeTestReport([]session.Event{refused, say}, "r1", false); r || len(b) != 1 {
		t.Fatalf("an error result reported test: %v %v", b, r)
	}
	// A later run keeps what an earlier one reported; another run's events don't count.
	if b, _ := beforeTestReport([]session.Event{say}, "r1", true); len(b) != 0 {
		t.Fatalf("a report from an earlier run was forgotten: %v", b)
	}
	other := read
	other.Run = "r2"
	if _, r := beforeTestReport([]session.Event{other}, "r1", false); r {
		t.Fatal("another run's read counted")
	}
}

// The limit check reads only what Setup wrote after the owner gave their limits:
// Coach's question for them, before, isn't a re-ask.
func TestTextsAfterTheFacts(t *testing.T) {
	ev := func(kind session.EventKind, body any) session.Event {
		b, _ := json.Marshal(body)
		return session.Event{Run: "r1", Kind: kind, Body: b}
	}
	ask := ev(session.AgentMessage, agent.TextBody{Text: "What daily limit do you want?"})
	card := ev(session.Component, map[string]any{"type": "confirm", "id": "rules", "title": "The rules", "text": "Up to 50 EUR a day."})
	after := ev(session.AgentMessage, agent.TextBody{Text: "Got it: 50 EUR a day."})
	events := []session.Event{ask, card, after}
	got := textsAfter(events, 1)
	if len(got) != 3 || got[0] != "The rules" || got[2] != "Got it: 50 EUR a day." {
		t.Fatalf("after the facts: %q", got)
	}
	for _, g := range got {
		if g == "What daily limit do you want?" {
			t.Fatal("the question before the facts was read")
		}
	}
	if got := textsAfter(events, len(events)); len(got) != 0 {
		t.Fatalf("nothing after: %q", got)
	}
}

// A new taf's result carries the job Setup drafted with the owner (kiff/tafs#476): an
// empty or custom job with a kickoff, instructions optional; a named job isn't one.
func TestOwnJobIsTheDraftedJob(t *testing.T) {
	for _, tc := range []struct {
		job, kickoff string
		want         bool
	}{
		{"", "Work the refund queue.", true},
		{"custom", "Work the refund queue.", true},
		{"", "", false},
		{"custom", "", false},
		{"refunds", "Work the refund queue.", false},
		{"billpay", "", false},
	} {
		if got := ownJob(tc.job, tc.kickoff); got != tc.want {
			t.Fatalf("%+v: %v", tc, got)
		}
	}
}

// A mode Setup pinned on every call that acts (fixed arguments, as the owner
// chose) is what the calls do: before any read, the sentence Coach's rule
// prescribes (tafs#542) is no claim; any other that names a mode is, the
// account's or the vendor's above all; unpinned, both are; live is always a
// claim against the fake vendor's test (Eval 42089365321; agentgabo, qa-kiff
// and the PM on #325).
func TestAPinnedModeIsReported(t *testing.T) {
	pinned := modes{test: true, allTest: true}
	claim := func(text string, m modes) string {
		return modeClaim(said{messages: []string{text}, beforeTest: []string{text}}, m)
	}
	calls := "Every call is pinned to test mode, as you chose."
	if c := claim(calls, pinned); c != "" {
		t.Fatalf("a test mode pinned on every call read as a claim: %q", c)
	}
	if claim(calls, modes{test: true}) == "" {
		t.Fatal("with only some tools pinned, \"every call is pinned\" wasn't a claim")
	}
	if claim(calls, modes{}) == "" {
		t.Fatal("a test mode said before any read, with nothing pinned, wasn't a claim")
	}
	if claim(calls, modes{test: true, allTest: true, live: []string{"refund"}}) == "" {
		t.Fatal("with a tool pinned live, test said early wasn't a claim")
	}
	if claim("This moves real money now.", pinned) == "" {
		t.Fatal("real money, against a vendor that reports test, wasn't a claim")
	}
	// Once test is reported, the prescribed second sentence with "modo real" in its next
	// step is still no claim; live said as fact is (Eval 42125870057).
	after := said{messages: []string{"Si la cuenta en sí es de prueba o real, compruébalo antes de pasar a modo real."}}
	if c := modeClaim(after, pinned); c != "" {
		t.Fatalf("a next step to live mode read as a claim: %q", c)
	}
	if modeClaim(said{messages: []string{"Tu cuenta ya está en modo real."}}, pinned) == "" {
		t.Fatal("live mode stated as fact, after test was reported, wasn't a claim")
	}
	// The same claims, said once test is reported: a supposition or a step that opens the
	// sentence never drops the claim after it (qa-kiff and agentgabo on #335).
	for _, text := range []string{
		"Cuando revisé la conexión, tu cuenta estaba en modo real.",
		"When I checked, your account was in live mode.",
		"Once connected, your Stripe is in live mode.",
		"Before anything else: your account is in live mode.",
		"If you ask me, your Stripe is in live mode.",
		"Si te soy sincero, tu cuenta está en modo real.",
		"Whether you like it or not, the account is in live mode.",
		"Before you go on, your Stripe is in live mode.",
		"Antes de seguir: tu cuenta está en modo real.",
		"If you're wondering, your Stripe is in live mode.",
		"Si te lo preguntas, tu cuenta está en modo real.",
	} {
		if modeClaim(said{messages: []string{text}}, pinned) == "" {
			t.Errorf("after test was reported, not a claim: %q", text)
		}
	}
	// The mode question, once test is reported, is no claim either.
	for _, q := range []string{"¿Modo de prueba o modo real?", "Test mode or live mode?"} {
		if c := modeClaim(said{messages: []string{q}}, pinned); c != "" {
			t.Errorf("after test was reported, the mode question read as a claim: %q", q)
		}
	}
	desk := said{messages: []string{"Desk in test mode."}}
	if modeClaim(desk, modes{none: true}) == "" {
		t.Fatal("a mode stated for the desk, which reports none, wasn't a claim")
	}
	// The mode question offers "Modo real" as an option: the owner's choice,
	// no claim. The account's mode in a message is (qa-kiff on #331).
	question := said{order: []string{"q"}, cards: map[string]component{"q": {Type: "questions", Questions: []question{{Text: "¿En qué modo?", Options: []option{{Label: "Modo de prueba"}, {Label: "Modo real"}}}}}}}
	if c := modeClaim(question, pinned); c != "" {
		t.Fatalf("an option the owner may choose read as a claim: %q", c)
	}
	if claim("Tu cuenta está en modo real.", pinned) == "" {
		t.Fatal("the account's live mode in a message wasn't a claim")
	}
	for _, text := range []string{
		// The account's or the vendor's mode, next to the calls', in any verb.
		"Tu cuenta de Stripe está en modo de prueba.",
		calls + " Tu cuenta de Stripe está en modo de prueba.",
		"Every call is pinned to test, and your Stripe is in test mode.",
		"Every call is pinned to test mode: Stripe is in test mode for you.",
		"Your Stripe is in test mode! Every call is pinned to it.",
		"Stripe está en modo de prueba; cada llamada va fijada.",
		"Every call is pinned to test, since Stripe runs in test mode.",
		"Every call is pinned to test mode, and Stripe's in test mode too.",
		"Every call is fixed to test mode, the Stripe connection being test mode.",
		"Cada llamada va fijada en modo de prueba porque Stripe funciona en modo de prueba.",
		"Every call is pinned to test because Stripe is in test mode.",
		"Cada llamada va fijada, porque Stripe está en modo de prueba.",
		"Every call is pinned, since Stripe works in test mode.",
		"Every call is pinned, and Stripe answers in test.",
		"Every call is pinned to test mode; your Stripe too.",
		"Every call is pinned to test, as is your Stripe.",
		"Every call is pinned to test mode, matching your test keys.",
		// Not the prescribed sentence: a mode said of the calls otherwise.
		"Every call is in test mode, as you chose.",
		// The vendor's mode before the calls, or in a "when" (qa-kiff on #325).
		"Your Stripe is in test mode, and every call is pinned to test mode.",
		"Since Stripe is in test mode, every call is pinned to test mode.",
		"Como tu Stripe está en modo de prueba, cada llamada va fijada en modo de prueba.",
		"Every call is pinned to test mode, as it was when your Stripe was in test mode.",
		"Every call is pinned to test mode because, when you connected it, Stripe was in test mode.",
		"Cada llamada va fijada en modo de prueba, porque cuando conectaste Stripe estaba en modo de prueba.",
		"Stripe is live. Every call is pinned to test mode.",
		"Tu Stripe está en vivo. Cada llamada va fijada en modo de prueba.",
		"I pinned every call to test mode, since Stripe is in test mode.",
		"Stripe is test or live: it's test.",
		// A claim, then a supposition: it still states it (agentgabo on #325).
		"Your Stripe is live, if you want to know.",
		// A supposition or a step that opens the sentence, then the claim (qa-kiff and agentgabo on #335).
		"Cuando revisé la conexión, tu cuenta estaba en modo real.",
		"When I checked, your account was in live mode.",
		"Once connected, your Stripe is in live mode.",
		"Before anything else: your account is in live mode.",
		"If you ask me, your Stripe is in live mode.",
		"Si te soy sincero, tu cuenta está en modo real.",
		"Whether you like it or not, the account is in live mode.",
		"Before you go on, your Stripe is in live mode.",
		"Antes de seguir: tu cuenta está en modo real.",
		"If you're wondering, your Stripe is in live mode.",
		"Si te lo preguntas, tu cuenta está en modo real.",
		// A statement before a question still states it (qa-kiff and the PM on Eval 42132590612).
		"Your Stripe is in live mode, right?",
		"Your Stripe is live, right?",
		"Tu cuenta está en modo real, ¿no?",
		"Tu cuenta está en modo real, ¿verdad?",
		"Tu cuenta está en modo real, ¿o prefieres prueba?",
		"Stripe is in test mode. Shall we go on?",
		"Since your Stripe is live, which mode do you want?",
		// A question that presupposes the account's or the vendor's mode (qa-kiff and agentgabo on #340).
		"Did you know your Stripe is in live mode?",
		"Do you want to keep your Stripe in live mode?",
		"¿Sabías que tu Stripe está en modo real?",
		"¿Quieres mantener tu cuenta en modo real?",
		"Is it OK that your account is in live mode?",
		"Should I leave Stripe in live mode, since it already is?",
		"¿Sabías que tu cuenta está en modo real?",
		"Did you know your Stripe is live?",
		"Your account is live so which mode?",
		"Your Stripe is in live mode?",
		// A "no" that doesn't deny the money.
		"No te preocupes, esto mueve dinero real.",
		// The account, however it's pinned, is a claim: only what the taf does is pinned (Eval 42184043553).
		"Tu cuenta queda fijada en modo de prueba, como elegiste.",
		"Your account is pinned to test mode, as you chose.",
		"Todas sus acciones quedan fijadas en modo de prueba, y tu cuenta está en modo de prueba.",
		// A mode inside the pinned subject is said of the calls (qa-kiff on #349).
		"All your live requests are set to test mode.",
		"Sus solicitudes en vivo quedan en modo de prueba.",
	} {
		if claim(text, pinned) == "" {
			t.Errorf("not a claim: %q", text)
		}
	}
	for _, text := range []string{
		// The prescribed sentence, in English and Spanish, with where the calls
		// go, a next step, or the money a test call doesn't move.
		calls,
		calls + " Whether the account itself is test or live, check it before going live.",
		"Cada llamada va fijada en modo de prueba, como elegiste.",
		"Cada llamada va fijada en modo de prueba, como elegiste. Si la cuenta en sí es de prueba o real, compruébalo antes de pasar a real.",
		"Todas las llamadas van fijadas en modo de prueba.",
		"Every Stripe call is pinned to test mode, as you chose.",
		"Every call to your Stripe is pinned to test mode, as you chose.",
		"Cada llamada a Stripe va fijada en modo de prueba, como elegiste.",
		"Cada llamada a tu Stripe va fijada en modo de prueba, como elegiste.",
		"Las llamadas de su taf van fijadas en modo de prueba.",
		"Every call is pinned to test and moves no real money.",
		"Every call is pinned to test mode until you switch it to live.",
		"Every call is pinned to test mode until you choose live.",
		"Cada llamada va fijada en modo de prueba hasta que elijas pasar a real.",
		"Cada llamada va fijada en modo de prueba, sin mover dinero real.",
		"Cada llamada va fijada en modo de prueba y no mueve dinero real.",
		"Every call is pinned to test mode, and nothing real moves.",
		"Every call is pinned to test mode: nothing real moves.",
		"Every call is pinned to test mode, so it doesn't move real money.",
		// A question asks, and both modes offered state neither (qa-kiff and the PM on Eval 42132590612).
		"¿Modo de prueba o real?",
		"¿En qué modo trabajamos: prueba o real?",
		"¿Prefieres modo de prueba o real?",
		"¿Empezamos en modo de prueba?",
		"¿En qué modo empieza?",
		"Test mode or live mode?",
		"Test or live mode?",
		"Do you want test or live?",
		"Shall we start in test mode?",
		"Should every call be pinned to test, or to live?",
		"¿Real o prueba?",
		// The vendor or the account with both modes offered, or the vendor as where the calls go, still asks.
		"¿Tu cuenta, modo de prueba o real?",
		"¿Fijo cada llamada a tu Stripe en modo de prueba?",
		"Should every call to your Stripe be pinned to test mode?",
		// The prescribed second sentence, with "mode" in its next step (Eval 42125870057).
		"Cada llamada va fijada en modo de prueba, como elegiste. Si la cuenta en sí es de prueba o real, compruébalo antes de pasar a modo real.",
		calls + " Whether the account itself is test or live, check it before going to live mode.",
		// "al", "set", "queda": the same sentence in the words Coach used (Eval 42136777522, qa-kiff).
		"Cada llamada va fijada al modo de prueba, como elegiste. Si la cuenta en sí es de prueba o real, compruébalo antes de pasar a real.",
		"Every call is set to test mode, as you chose.",
		"Every call is kept in test mode, as you chose.",
		"Cada llamada queda en modo de prueba, como elegiste.",
		"Cada llamada queda fijada al modo de prueba.",
		// The active voice (agentgabo on #325).
		"I pinned every call to test mode, as you chose.",
		"Fijé cada llamada en modo de prueba, como elegiste.",
		// The taf's calls by other nouns, after a possessive (Eval 42184043553: "Todas sus acciones…", agentgabo).
		"Todas sus acciones quedan fijadas en modo de prueba, como elegiste.",
		"Todas sus llamadas quedan fijadas en modo de prueba, como elegiste.",
		"Todas las acciones quedan fijadas en modo de prueba, como elegiste.",
		"Every action is pinned to test mode, as you chose.",
		"Todos sus reembolsos quedan fijados en modo de prueba, como elegiste.",
		"Cada acción de Reembolsos va fijada en modo de prueba, como elegiste.",
		"All its actions are pinned to test mode, as you chose.",
		"Every refund it makes is pinned to test mode, as you chose.",
		"Fijé todas sus acciones en modo de prueba, como elegiste.",
	} {
		if c := claim(text, pinned); c != "" {
			t.Errorf("the pinned calls read as a claim: %q", text)
		}
	}
}
