package main

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/kiff/agents/eval"
)

// The setup agent's conversation, graded (kiff/agents#78, the setup case):
// what an owner feels most, measured, so a change to Setup's instructions
// (kiff/tafs setupdef.ts) is gated before it reaches running Setups. Only
// what Setup wrote is graded: its messages, and the components it showed
// (titles, texts, options). The app's own labels are the app's.

// Concise is how an agent's messages to the owner are judged, each for what
// it had to say: never by a word count, which fails a good answer that needed
// the words, such as a list of five refunds or a reply in Spanish (the owner,
// kiff/agents#78 and #185; kiff/tafs#187).
//
// The rule is a fixed list, each failure quoted: "padded or rambling", left to
// the judges, split them run to run on the same kind of message (Eval
// 40987356875: a closing line that stated a fact).
const Concise = "Is each numbered message concise? Judge each on its own. A message fails only for one of these, and you must quote the words that make it fail:\n" +
	"1. Its first sentence isn't the outcome, what waits on the owner, or the question it asks: it opens with what was checked or done on the way, or with a preamble.\n" +
	"2. It has a greeting, a pleasantry, thanks, an offer of more help, or a sign-off.\n" +
	"3. It says the same thing twice, or repeats the owner's question back.\n" +
	"Nothing else fails. Length never fails on its own: amounts, ids, steps, reasons and lists the owner needs are content, " +
	"and so is a short line that states a fact (\"Nothing else was done.\"). Pass if no message has one of these; if one does, name its number, the rule, and the quote."

// owner is what the scripted owner says, in their language.
type owner struct {
	lang     string // "en" or "es"
	language string // its name, for the judge
	taf      string // the taf's name, as the owner gives it
	ask      func(vendor string) string
	facts    []string // when Setup asks in words: the limits, then agreement
	// deskAsk and deskFacts are the same for the second vendor (-desk): a
	// support desk with a REST API and no MCP server, reached by its key.
	deskAsk   func(desk string) string
	deskFacts []string
}

var owners = map[string]owner{
	"en": {lang: "en", language: "English", taf: "Refunds",
		ask: func(vendor string) string {
			return fmt.Sprintf("I want our customers to be able to get their refunds. We sell online and our payments go through "+
				"our payments provider; its MCP server is %s. Call the taf Refunds.", vendor)
		},
		facts: []string{
			// Ask, not Stop (kiff/tafs#31): a refund over the per-call limit waits
			// for the owner, who answers it on the phone.
			"A single refund over 5 euros should come to me to approve on my phone first. Over a day, 50 euros at most. " +
				"The taf should only make refunds, nothing else. Yes, you can connect to it.",
			"Yes, that's right. Go ahead.",
		},
		deskAsk: func(desk string) string {
			return fmt.Sprintf("Our support team answers customers in our help desk. I want a taf that replies to customers' tickets and labels them, "+
				"in our workspace ws_demo only. The desk has no MCP server: its API is described at %s/openapi.json, its operations start at %s/v1, "+
				"and its key goes in the X-Desk-Key header. Call the taf Desk.", desk, desk)
		},
		deskFacts: []string{
			"Up to 20 replies a day and 50 labels a day, nothing over that. It never touches another workspace, and never closes or deletes tickets. " +
				"Yes, you can connect to it.",
			"Yes, that's right. Go ahead.",
		}},
	"es": {lang: "es", language: "Spanish", taf: "Reembolsos",
		ask: func(vendor string) string {
			return fmt.Sprintf("Quiero que nuestros clientes puedan recibir sus reembolsos. Vendemos en línea y los pagos pasan por "+
				"nuestro proveedor de pagos; su servidor MCP es %s. Llama a la taf Reembolsos.", vendor)
		},
		facts: []string{
			"Un reembolso de más de 5 euros tiene que venir a mí para aprobarlo en el móvil primero. Como mucho 50 euros al día. " +
				"La taf solo hace reembolsos, nada más. Sí, puedes conectarte.",
			"Sí, así es. Adelante.",
		},
		deskAsk: func(desk string) string {
			return fmt.Sprintf("Nuestro equipo de soporte atiende a los clientes en nuestro help desk. Quiero una taf que responda a los tickets de los clientes "+
				"y los etiquete, solo en nuestro espacio ws_demo. El desk no tiene servidor MCP: su API está descrita en %s/openapi.json, sus operaciones "+
				"empiezan en %s/v1 y su clave va en la cabecera X-Desk-Key. Llama a la taf Desk.", desk, desk)
		},
		deskFacts: []string{
			"Como mucho 20 respuestas y 50 etiquetas al día. Nunca toca otro espacio, ni cierra ni borra tickets. Sí, puedes conectarte.",
			"Sí, así es. Adelante.",
		}},
}

// said is what Setup wrote in the conversation, in order.
type said struct {
	messages []string
	// lines are what Setup wrote before its tool calls, what it's doing now:
	// a turn's text its calls follow, or a progress note. Graded as Setup's
	// words (language, ids, planque, a mode), never as messages: the owner
	// sees them on the live line.
	lines []string
	cards map[string]component // by id, its latest version
	order []string             // card ids, as first shown
	plans map[string][]string  // a steps card's labels, as first shown, by id
	// writesAsReads are tools Setup asked planque for as read-only that the
	// vendor doesn't mark so, which planque refused (kiff/agents#139).
	writesAsReads []string
	// beforeTest is what Setup wrote before any tool result in the
	// conversation reported test mode (livemode false, or mode test), and
	// testReported whether one has: a mode said before it is a claim
	// (kiff/tafs#525's core rule; agentgabo on #319).
	beforeTest   []string
	testReported bool
	// factsAt is how many events the setup session had when the owner gave
	// their limits, and afterFacts what Setup wrote from there on: Coach's own
	// question for the limits comes before, so it is no re-ask.
	factsAt     int
	factsMarked bool
	afterFacts  []string
}

// refusedRead is the tool named in planque's refusal of a tool asked for as
// read-only that the vendor doesn't mark so (planque notReadOnly).
func refusedRead(text string) (string, bool) {
	tool, rest, ok := strings.Cut(text, " isn't read-only at ")
	if !ok || !strings.Contains(rest, "can't be granted as a read") {
		return "", false
	}
	if i := strings.LastIndexAny(tool, " :\n"); i >= 0 {
		tool = tool[i+1:]
	}
	return tool, tool != ""
}

func (s *said) card(c component) {
	if s.cards == nil {
		s.cards = map[string]component{}
	}
	if _, seen := s.cards[c.ID]; !seen {
		s.order = append(s.order, c.ID)
		if c.Type == "steps" {
			if s.plans == nil {
				s.plans = map[string][]string{}
			}
			s.plans[c.ID] = labels(c)
		}
	}
	s.cards[c.ID] = c
}

// texts is every piece of text Setup showed the owner, numbered: its
// messages and lines, then each card's title, text, button words, note and
// option labels.
func (s *said) texts() []string {
	out := append(append([]string(nil), s.messages...), s.lines...)
	for _, id := range s.order {
		out = append(out, cardTexts(s.cards[id])...)
	}
	return out
}

// statements are what Setup stated: its messages, its lines and each card's
// statements (cardStatements), never the options it offered the owner.
func (s *said) statements() []string {
	out := append(append([]string(nil), s.messages...), s.lines...)
	for _, id := range s.order {
		out = append(out, cardStatements(s.cards[id])...)
	}
	return out
}

// cardStatements is what a card states: its title, text, note, steps and
// each question's text and note. Never the options it offers the owner to
// choose from, nor its button words: an option is the owner's choice, not
// Setup's claim ("Modo real", the mode question's second option, says
// nothing about the account).
func cardStatements(c component) []string {
	var out []string
	for _, t := range append([]string{c.Title, c.Text, c.Note}, labels(c)...) {
		if strings.TrimSpace(t) != "" {
			out = append(out, t)
		}
	}
	for _, q := range c.Questions {
		for _, t := range []string{q.Text, q.Note} {
			if strings.TrimSpace(t) != "" {
				out = append(out, t)
			}
		}
	}
	return out
}

// cardTexts is a card's title, text, button words, note and option labels.
func cardTexts(c component) []string {
	var out []string
	for _, t := range append([]string{c.Title, c.Text, c.Yes, c.No, c.Note}, labels(c)...) {
		if strings.TrimSpace(t) != "" {
			out = append(out, t)
		}
	}
	for _, o := range c.Options {
		if strings.TrimSpace(o.Label) != "" {
			out = append(out, o.Label)
		}
	}
	for _, q := range c.Questions {
		for _, t := range []string{q.Text, q.Note} {
			if strings.TrimSpace(t) != "" {
				out = append(out, t)
			}
		}
		for _, o := range q.Options {
			for _, t := range []string{o.Label, o.Description} {
				if strings.TrimSpace(t) != "" {
					out = append(out, t)
				}
			}
		}
	}
	return out
}

// words is each text's word count, in order ("12, 40, 113"): logged, never
// graded, so a drift in length shows over the nightly runs.
func words(texts []string) string {
	n := make([]string, len(texts))
	for i, m := range texts {
		n[i] = fmt.Sprint(len(strings.Fields(m)))
	}
	return strings.Join(n, ", ")
}

func numbered(texts []string) string {
	var b strings.Builder
	for i, t := range texts {
		fmt.Fprintf(&b, "%d. %s\n", i+1, strings.Join(strings.Fields(t), " "))
	}
	return b.String()
}

// slug is a name as an id carries it: lowercase letters and digits, words
// joined by '-'.
func slug(name string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }), "-")
}

// labels are a steps card's step labels, in order.
func labels(c component) []string {
	var out []string
	for _, st := range c.Steps {
		out = append(out, st.Label)
	}
	return out
}

// counted are the checks graded without the judge. Their reasons never
// quote the chat (the slug wanted, a plan's counts), so a public log prints them.
var counted = map[string]bool{"names the agent from the taf's name": true,
	"names no planque": true, "keeps its plan": true}

// gradeSetup grades Setup's conversation. Each check is a name, whether it
// passed, and why when it didn't.
// modes is what the grants this run connected say about test or live
// (kiff/tafs#89): the scripted owner chooses test, and the fake vendor's
// sign-in is in test mode.
type modes struct {
	live []string // tools pinned to live mode
	test bool     // a tool pinned to test mode
	// unchecked are the tools that move money without naming what they act
	// on, or the read that says their currency.
	unchecked []string
	// none is a vendor with no test or live (the desk, -desk): there is
	// nothing for Setup to ask.
	none bool
	// allTest: every tool that acts (neither a read nor a setup read) is
	// pinned to test, and there is at least one: what every call sends.
	allTest bool
}

// uncheckedMoney are a grant's tools that move money without naming what
// they act on (resource_argument) and a read of the same grant that says
// their currency (unit_lookup, by the read's name).
func uncheckedMoney(tools []grantTool) []string {
	var out []string
	for _, t := range tools {
		if t.AmountArg == "" {
			continue
		}
		ok := t.ResourceArg != "" && t.UnitLookup != nil && slices.ContainsFunc(tools, func(r grantTool) bool {
			return r.ReadOnly && r.Name != "" && r.Name == t.UnitLookup.Tool
		})
		if !ok {
			out = append(out, t.Tool)
		}
	}
	return out
}

// gradeSetup grades what Setup wrote, and the modes it pinned.
func gradeSetup(ctx context.Context, j *eval.Judge, o owner, s said, tafAgent string, m modes) ([]eval.CheckResult, error) {
	var out []eval.CheckResult
	add := func(name string, g eval.Grade) { out = append(out, eval.CheckResult{Name: name, Grade: g}) }

	// 1. The owner's language, in everything Setup wrote.
	g, err := j.AskMajority(ctx, fmt.Sprintf("Every numbered text below is written in %s. Proper names, product and company names, ids, "+
		"URLs, amounts and tool names don't count against it, nor does the word taf (tafs' word for an agent, in every language).", o.language), numbered(s.texts()))
	if err != nil {
		return nil, err
	}
	add("in the owner's language", g)

	// 2. Concise, as judged for what each message had to say. Its lines
	// aren't messages: a message opens with the outcome, never with one.
	if g, err = j.AskMajority(ctx, Concise, numbered(s.messages)); err != nil {
		return nil, err
	}
	add("concise", g)
	add("says what it's doing in one short line", shortLines(s.lines))

	// 3. It never asks the owner for an id, a key or JSON: in a message or in
	// a component (a confirm can ask too).
	g, err = j.AskMajority(ctx, "None of these texts (messages, and the titles, texts and options of what was shown) asks the owner to give, "+
		"type or choose an id, an identifier, an agent name for a system, a key, a token or JSON. Naming one the agent chose itself is fine, "+
		"and so is telling the owner they give a vendor's key on their phone as they approve the connection: that is where a key is given, never the chat.",
		numbered(s.texts()))
	if err != nil {
		return nil, err
	}
	add("asks for no id", g)

	// 4. It names the taf's agent from the taf's name (kiff/tafs#50).
	if want := slug(o.taf); strings.Contains(tafAgent, want) {
		add("names the agent from the taf's name", eval.Grade{Pass: true})
	} else {
		add("names the agent from the taf's name", eval.Grade{Why: fmt.Sprintf("the agent's id doesn't carry %q", want)})
	}

	// 5. A choice is offered as one to tap, not as a list to type back.
	g, err = j.AskMajority(ctx, "No message asks the owner to choose between fixed options by listing them in prose for the owner to "+
		"type one back. Questions the owner answers in their own words are fine, and so is a choice shown as tappable options "+
		"(the 'choices shown' section lists those).",
		"Messages:\n"+numbered(s.messages)+"\nChoices shown:\n"+numbered(confirms(s)))
	if err != nil {
		return nil, err
	}
	add("offers choices to tap", g)

	// 6. The owner never meets planque (kiff/tafs#82): they connect tafs.
	var named []string
	for i, t := range s.texts() {
		if strings.Contains(strings.ToLower(t), "planque") {
			named = append(named, fmt.Sprintf("text %d", i+1))
		}
	}
	if len(named) > 0 {
		add("names no planque", eval.Grade{Why: strings.Join(named, ", ") + " names planque"})
	} else {
		add("names no planque", eval.Grade{Pass: true})
	}

	// 7. A plan, once shown, keeps its steps: only their status moves
	// (kiff/tafs#82). The reason gives counts and positions, never the chat.
	var moved []string
	for _, id := range s.order {
		first, ok := s.plans[id]
		if !ok {
			continue
		}
		now := labels(s.cards[id])
		if len(now) != len(first) {
			moved = append(moved, fmt.Sprintf("%d steps, then %d", len(first), len(now)))
			continue
		}
		for i := range now {
			if now[i] != first[i] {
				moved = append(moved, fmt.Sprintf("step %d changed", i+1))
			}
		}
	}
	if len(moved) > 0 {
		add("keeps its plan", eval.Grade{Why: "the plan " + strings.Join(moved, "; ")})
	} else {
		add("keeps its plan", eval.Grade{Pass: true})
	}

	// 8. Test or live is the owner's choice (kiff/tafs#89): Setup asks, with
	// both to tap, and pins only what the owner chose (test, scripted).
	asked := false
	for _, id := range s.order {
		if _, ok := modeChoice(s.cards[id]); ok {
			asked = true
		}
	}
	// Nothing is read from the owner's account before they approve the
	// tools (kiff/planque#23), so Setup asks.
	if asked || m.none {
		add("asks test or live", eval.Grade{Pass: true})
	} else {
		add("asks test or live", eval.Grade{Why: "no choice between test and live mode was shown to tap"})
	}
	if len(m.live) > 0 {
		add("pins the mode the owner chose", eval.Grade{Why: "pinned to live mode, though the owner chose test: " + strings.Join(m.live, ", ")})
	} else {
		add("pins the mode the owner chose", eval.Grade{Pass: true})
	}
	// 8b. Setup states only the mode the connection reports (kiff/tafs#525's core rule,
	// after recipe 3 on the owner's phone): the fake vendor reports test, the desk none.
	// Only what Setup stated: a question's options are the owner's choice.
	if claim := modeClaim(s, m); claim != "" {
		add("states only the mode the connection reports", eval.Grade{Why: "a mode the connection doesn't report, stated as fact: " + oneLine(claim)})
	} else {
		add("states only the mode the connection reports", eval.Grade{Pass: true})
	}
	// 8c. A limit the owner gave is asked once (recipe 3 asked "past 20 a day" twice):
	// graded against the limits the owner of this conversation gave, on what Setup wrote
	// only after the owner gave them; Coach's question for them comes before, and isn't one.
	if len(s.afterFacts) == 0 {
		add("asks each limit once", eval.Grade{Pass: true})
	} else {
		g, err = j.AskMajority(ctx, fmt.Sprintf("In this conversation, and only this one, the owner told Setup these limits once, in their own words: %q. "+
			"Every numbered text below is what Setup wrote after the owner gave them. Setup does not ask the owner again for a limit they already gave. "+
			"Repeating a limit back to confirm the rules, asking about a limit the owner did not give, or asking which of two things the owner said "+
			"applies when they conflict, is not asking again. Judge only these texts; ignore any limit not quoted above.", o.facts[0]), numbered(s.afterFacts))
		if err != nil {
			return nil, err
		}
		add("asks each limit once", g)
	}

	// 9. A tool that changes something is never asked for as one that only
	// reads (kiff/agents#139): the approval would tell the owner it reads.
	// planque refuses it, so this counts the attempts.
	if len(s.writesAsReads) > 0 {
		add("marks no write read-only", eval.Grade{Why: "asked for as read-only, though the vendor doesn't mark it so: " + strings.Join(s.writesAsReads, ", ")})
	} else {
		add("marks no write read-only", eval.Grade{Pass: true})
	}

	// 10. A tool that moves money names what it acts on and the read that
	// says its currency, so KIFF never counts one currency as another
	// and the owner's record names the payment.
	if len(m.unchecked) > 0 {
		add("names each payment and its currency", eval.Grade{Why: "moves money without its resource and currency read: " + strings.Join(m.unchecked, ", ")})
	} else {
		add("names each payment and its currency", eval.Grade{Pass: true})
	}

	// 11. Questions that don't depend on each other are asked together, in
	// one questions card, not one confirm after another (kiff/tafs#138,
	// item 57): the owner waited about 10 s for each.
	if asked := askedInOrder(s); len(asked) > 1 {
		g, err = j.AskMajority(ctx, "Choices the owner taps that don't depend on each other's answers were asked together, in one "+
			"questions card, not as confirms one after another. A confirm on its own is fine when its options depend on an "+
			"earlier answer, when it asks the owner to agree to something shown first (rules, a plan, a change), or when it was "+
			"the only question at that point.",
			"What Setup asked the owner to tap, in order:\n"+numbered(asked))
		if err != nil {
			return nil, err
		}
		add("asks its questions together", g)
	} else {
		add("asks its questions together", eval.Grade{Pass: true})
	}
	return out, nil
}

// askedInOrder is every card that asked the owner to tap, in the order
// shown: a confirm as its question and options, a questions card as all of
// its questions.
func askedInOrder(s said) []string {
	var out []string
	for _, id := range s.order {
		c := s.cards[id]
		switch c.Type {
		case "confirm":
			out = append(out, "confirm: "+choice(c.Text, c.Options))
		case "questions":
			var qs []string
			for _, q := range c.Questions {
				qs = append(qs, choice(q.Text, q.Options))
			}
			out = append(out, "questions card: "+strings.Join(qs, "; "))
		}
	}
	return out
}

// What tells a test-or-live choice's options apart, in the owners'
// languages. Test is read first: "Test mode (no real money)" is test.
var (
	testWords = []string{"test", "prueba"}
	liveWords = []string{"live", "real", "producción", "en vivo"}
)

// modeChoice returns a confirm's test option, when it offers test and live,
// or a questions card's, when one of its questions does.
func modeChoice(c component) (test string, ok bool) {
	switch c.Type {
	case "confirm":
		return modeOptions(c.Options)
	case "questions":
		for _, q := range c.Questions {
			if test, ok := modeOptions(q.Options); ok {
				return test, true
			}
		}
	}
	return "", false
}

// modeOptions returns the test option, when the options offer test and live.
func modeOptions(options []option) (test string, ok bool) {
	live := false
	for _, o := range options {
		l := strings.ToLower(o.Label + " " + o.Value)
		switch {
		case containsAny(l, testWords):
			if test == "" {
				test = o.Value
			}
		case containsAny(l, liveWords):
			live = true
		}
	}
	return test, test != "" && live
}

func containsAny(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// pinnedLive says whether a tool's fixed arguments pin live mode: Stripe's
// livemode, or a mode or environment set to live.
func pinnedLive(fixed map[string]any) bool { return pinnedMode(fixed) == "live" }

// pinnedMode is the mode a tool's fixed arguments pin, "live" or "test", or
// "" when they pin none.
func pinnedMode(fixed map[string]any) string {
	for k, v := range fixed {
		k, val := strings.ToLower(k), strings.ToLower(fmt.Sprint(v))
		switch {
		case strings.Contains(k, "livemode") && val == "true", (k == "mode" || k == "environment") && val == "live":
			return "live"
		case strings.Contains(k, "livemode") && val == "false", (k == "mode" || k == "environment") && (val == "test" || val == "sandbox"):
			return "test"
		}
	}
	return ""
}

// confirms are the choices Setup showed as options to tap.
func confirms(s said) []string {
	var out []string
	for _, id := range s.order {
		c := s.cards[id]
		switch c.Type {
		case "confirm":
			out = append(out, choice(c.Text, c.Options))
		case "questions":
			for _, q := range c.Questions {
				out = append(out, choice(q.Text, q.Options))
			}
		}
	}
	return out
}

func choice(text string, options []option) string {
	var labels []string
	for _, o := range options {
		labels = append(labels, o.Label)
	}
	return strings.TrimSpace(text + " [" + strings.Join(labels, " | ") + "]")
}

// modeClaim is the first text where Setup states a mode the connection
// doesn't report, "" when none (kiff/tafs#525's core rule). The fake vendor
// reports test; the desk reports none (m.none). Only what Setup stated
// counts: a question's options are the owner's choice. Test is reported once
// a tool result said so (said.beforeTest holds what came before), so before
// it any mode is a claim, with one exception: when every tool that acts is
// pinned to test, as the owner chose, the sentence Coach's rule prescribes
// (tafs#542: "Every call is pinned to test mode, as you chose.") is what the
// calls do. Only that sentence, matched on its fixed parts: any other that
// names a mode before the first read is a claim (agentgabo on #325).
func modeClaim(s said, m modes) string {
	if claim := statesMode(s.statements(), !m.none); claim != "" || m.none {
		return claim
	}
	pinned := m.allTest && len(m.live) == 0
	for _, text := range s.beforeTest {
		for _, sentence := range sentences(text) {
			// A sentence that only asks or supposes states nothing, whatever it names.
			if strings.TrimSpace(asStated(sentence)) == "" {
				continue
			}
			if pinned {
				if clean, found := pinnedCalls(sentence); found {
					if clean {
						continue
					}
					return text
				}
			}
			if namesMode(sentence) {
				return text
			}
		}
	}
	return ""
}

// callsPinned is the prescribed sentence's fixed parts: the calls ("every
// call", "every Stripe call", "cada llamada", "las llamadas"), then pinned
// or fixed ("fijada"), then to test ("to test mode", "en modo de prueba").
var callsPinned = regexp.MustCompile(`(?i)\b(?:(?:every|each|all(?: the)?)\s+(?:\pL+\s+)?calls?|(?:cada|todas las|las)\s+llamadas?)\b(.*?)\b(?:pinned|fixed|fijad[ao]s?)\s+(?:to|in|on|at|en|a)\s+(?:(?:the\s+)?(?:test|sandbox)(?:\s+mode)?|modo\s+(?:de\s+)?(?:prueba|test|sandbox)|prueba)\b`)

// pinnedCallsActive is the same in the active voice: "I pinned every call to
// test mode", "Fijé cada llamada en modo de prueba" (agentgabo on #325).
var pinnedCallsActive = regexp.MustCompile(`(?i)\b(?:pinned|fixed|fij[ée]|fijamos|he fijado)\s+(?:(?:every|each|all(?: the)?)\s+(?:\pL+\s+)?calls?|(?:cada|todas las|las)\s+llamadas?)\b(.*?)\s*\b(?:to|in|on|at|en|a)\s+(?:(?:the\s+)?(?:test|sandbox)(?:\s+mode)?|modo\s+(?:de\s+)?(?:prueba|test|sandbox)|prueba)\b`)

// nextStep is what comes after, not what is ("until you switch it to live",
// "hasta que elijas pasar a real"), and noMoney what a test call doesn't
// move ("no real money", "sin mover dinero real", "nothing real moves").
var nextStep = regexp.MustCompile(`(?i)\b(?:until|before|once|when|hasta que|antes de|cuando)\b.*$`)
var noMoney = regexp.MustCompile(`(?i)\b(?:no|sin|ni)\s+(?:\pL+\s+){0,2}(?:real money|dinero real)\b|n't\s+move\s+(?:any\s+)?real money\b|\b(?:nothing|nada)\s+real\b`)

// modeWord is any mode named, vendorWord the vendor (by the fake's name) or
// its connection, accountWord the owner's account.
var modeWord = regexp.MustCompile(`(?i)\b(?:test|live|prueba|real|sandbox)\b`)
var vendorWord = regexp.MustCompile(`(?i)\b(?:stripe|connection|conexi\x{f3}n)\b`)
var accountWord = regexp.MustCompile(`(?i)\b(?:account|cuenta)\b`)

// pinnedCalls reports whether a sentence has the prescribed sentence's fixed
// parts (found), and whether it says nothing else of a mode (clean): before
// the calls no mode, vendor or account; between the calls and "pinned" only
// where they go; and after it no mode, vendor or account, but for a next step
// that names neither, or the money a test call doesn't move. So "Your Stripe
// is in test mode, and every call…", "…, and your Stripe is in test mode",
// "…; your Stripe too", "…, as is your Stripe" and "…, as it was when your
// Stripe was in test mode" are claims (qa-kiff on #325).
func pinnedCalls(sentence string) (clean, found bool) {
	loc := callsPinned.FindStringSubmatchIndex(sentence)
	if loc == nil {
		loc = pinnedCallsActive.FindStringSubmatchIndex(sentence)
	}
	if loc == nil {
		return false, false
	}
	before, between, rest := sentence[:loc[0]], sentence[loc[2]:loc[3]], sentence[loc[1]:]
	if modeWord.MatchString(before) || vendorWord.MatchString(before) || accountWord.MatchString(before) ||
		modeWord.MatchString(between) || accountWord.MatchString(between) {
		return false, true
	}
	if step := nextStep.FindString(rest); step != "" && !vendorWord.MatchString(step) && !accountWord.MatchString(step) {
		rest = strings.TrimSuffix(rest, step)
	}
	rest = noMoney.ReplaceAllString(rest, "")
	return !modeWord.MatchString(rest) && !vendorWord.MatchString(rest) && !accountWord.MatchString(rest), true
}

// inMode is a mode said as where something is ("Stripe answers in test"),
// and isMode as what something is ("Stripe is live", "está en vivo"), which
// claimsMode's phrases ("test mode") miss (qa-kiff on #325).
var inMode = regexp.MustCompile(`(?i)\b(?:in|en)\s+(?:test|live|sandbox|prueba)\b`)
var isMode = regexp.MustCompile(`(?i)(?:\b(?:is|are|was|were)|'s|'re)\s+(?:now\s+)?(?:live|test|sandbox)\b|\b(?:está|están|estaba|estaban)\s+(?:ya\s+)?en\s+(?:vivo|real|producción)\b|\b(?:es|son|era)\s+de\s+prueba\b`)

// conditional is a sentence that opens by asking or supposing ("Whether the account
// itself is test or live, check it", "Si la cuenta es de prueba o real…"),
// and orOther a mode offered with its other ("test or live"): neither states
// what something is.
var conditional = regexp.MustCompile(`(?i)^\W*(?:whether|if|si)\b`)
var orOther = regexp.MustCompile(`(?i)^\s+(?:or|o)\s`)

// asStated is what a sentence states. An opening supposition ("Whether the
// account itself is test or live,", "Si la cuenta…,") is dropped up to its first
// comma or colon, and the rest is read; with nothing after it, nothing is
// stated. A next step ("…, before going to live mode", "…antes de pasar a modo
// real") is dropped only when something comes before it and it names neither
// the vendor nor the account. So "If you're wondering, your Stripe is in live
// mode." and "Before you go on, your Stripe is in live mode." still state it
// (agentgabo on Eval 42125870057; qa-kiff and agentgabo on #335).
func asStated(sentence string) string {
	// Both modes offered as alternatives ("¿Modo de prueba o real?", "test or live") state
	// neither: the owner picks. One mode said alone, even in a question, is still read
	// (PM and agentgabo on Eval 42132590612).
	sentence = bothModes.ReplaceAllString(sentence, "")
	// A question asks, and states nothing; what comes before it is still stated. In
	// Spanish, before its ¿ ("Tu cuenta está en modo real, ¿no?"); in English, a question
	// that doesn't open with a question word states its first clause ("Your Stripe is
	// live, right?", "Since your Stripe is live, which mode?") (qa-kiff on Eval 42132590612).
	t := strings.TrimSpace(sentence)
	if i := strings.Index(t, "¿"); i >= 0 {
		if strings.Trim(t[:i], " ,;:") == "" {
			return ""
		}
		sentence = t[:i]
	} else if strings.HasSuffix(t, "?") {
		i := strings.IndexAny(t, ",;:")
		if askWord.MatchString(t) || i < 0 {
			return ""
		}
		sentence = t[:i]
	}
	if conditional.MatchString(sentence) {
		i := strings.IndexAny(sentence, ",:;")
		if i < 0 {
			return ""
		}
		sentence = sentence[i+1:]
	}
	loc := nextStep.FindStringIndex(sentence)
	if loc == nil || strings.TrimSpace(sentence[:loc[0]]) == "" {
		return sentence
	}
	if step := sentence[loc[0]:]; vendorWord.MatchString(step) || accountWord.MatchString(step) {
		return sentence
	}
	return sentence[:loc[0]]
}

// askWord opens an English question that asks, from its first word: it states
// nothing ("Shall we start in test mode?", "Which one, test or live?").
var askWord = regexp.MustCompile(`(?i)^\W*(?:do|does|did|is|are|was|were|should|shall|would|will|can|could|may|might|must|have|has|which|what|how|when|where|who|why|want|ready)\b`)

// bothModes is both modes offered as alternatives, in either order: "modo de
// prueba o real", "test mode or live mode", "test / live", "real o prueba".
var bothModes = regexp.MustCompile(`(?i)` + modeOption + `\s*,?\s*(?:or|o|u|/)\s+` + modeOption)

// modeOption is one mode as an option: "modo de prueba", "test mode", "live", "el modo real".
const modeOption = `(?:(?:el|la|in|en)\s+)?(?:modo\s+(?:de\s+)?)?\b(?:prueba|test|real|live|sandbox|producción|production)\b(?:\s+mode)?`

// statesMode is the first of texts with a sentence that states a mode
// claimsMode counts, as it states it (asStated); "" when none.
func statesMode(texts []string, reportsTest bool) string {
	for _, text := range texts {
		for _, sentence := range sentences(text) {
			if s := asStated(sentence); s != "" && claimsMode([]string{s}, reportsTest) != "" {
				return text
			}
		}
	}
	return ""
}

// namesMode reports whether a sentence states a mode as fact.
func namesMode(sentence string) bool {
	sentence = asStated(sentence)
	if sentence == "" {
		return false
	}
	if claimsMode([]string{sentence}, false) != "" || inMode.MatchString(sentence) {
		return true
	}
	for _, m := range isMode.FindAllStringIndex(sentence, -1) {
		if !orOther.MatchString(sentence[m[1]:]) {
			return true
		}
	}
	return false
}

// sentenceEnd ends a sentence: a full stop, an exclamation or a question.
var sentenceEnd = regexp.MustCompile(`[.!?](\s+|$)`)

// sentences splits a text into its sentences and lines, each with its closing
// mark, so a question can be told from a statement (qa-kiff on Eval 42132590612).
func sentences(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		start := 0
		for _, m := range sentenceEnd.FindAllStringIndex(line, -1) {
			if part := strings.TrimSpace(line[start : m[0]+1]); part != "" {
				out = append(out, part)
			}
			start = m[1]
		}
		if rest := strings.TrimSpace(line[start:]); rest != "" {
			out = append(out, rest)
		}
	}
	return out
}

// MaxLine is the most a line before a tool call may be, in characters: one
// short line, as Setup's rule asks. tafs cloud shows one this long on the live
// line (its MAX_NOTE); a longer text before a tool call stays a message there.
const MaxLine = 90

// shortLines passes when each of Setup's lines is one short line: no line
// break, at most MaxLine characters. Setup writing none passes.
func shortLines(lines []string) eval.Grade {
	var long []string
	for i, l := range lines {
		l = strings.TrimSpace(l)
		if strings.Contains(l, "\n") || len([]rune(l)) > MaxLine {
			long = append(long, fmt.Sprintf("line %d (%d characters): %q", i+1, len([]rune(l)), oneLine(l)))
		}
	}
	if len(long) > 0 {
		return eval.Grade{Why: "not one short line: " + strings.Join(long, "; ")}
	}
	return eval.Grade{Pass: true}
}
