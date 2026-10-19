package main

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/kiff/agents/eval"
)

// The setup agent's conversation, graded (kiff/agents#78, the setup case):
// what an owner feels most, measured, so a change to Setup's instructions
// (kiff/tafs setupdef.ts) is gated before it reaches running Setups. Only
// what Setup wrote is graded: its messages, and the components it showed
// (titles, texts, options). The app's own labels are the app's.

// maxWords bounds one of Setup's messages: "one or two short sentences".
const maxWords = 60

// owner is what the scripted owner says, in their language.
type owner struct {
	lang     string // "en" or "es"
	language string // its name, for the judge
	taf      string // the taf's name, as the owner gives it
	ask      func(vendor string) string
	facts    []string // when Setup asks in words: the limits, then agreement
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
			"A single refund over 5 euros (500 cents) should come to me to approve on my phone first. Over a day, 50 euros at most. " +
				"The taf should only make refunds, nothing else. Yes, you can connect to it.",
			"Yes, that's right. Go ahead.",
		}},
	"es": {lang: "es", language: "Spanish", taf: "Reembolsos",
		ask: func(vendor string) string {
			return fmt.Sprintf("Quiero que nuestros clientes puedan recibir sus reembolsos. Vendemos en línea y los pagos pasan por "+
				"nuestro proveedor de pagos; su servidor MCP es %s. Llama a la taf Reembolsos.", vendor)
		},
		facts: []string{
			"Un reembolso de más de 5 euros (500 céntimos) tiene que venir a mí para aprobarlo en el móvil primero. Como mucho 50 euros al día. " +
				"La taf solo hace reembolsos, nada más. Sí, puedes conectarte.",
			"Sí, así es. Adelante.",
		}},
}

// said is what Setup wrote in the conversation, in order.
type said struct {
	messages []string
	cards    map[string]component // by id, its latest version
	order    []string             // card ids, as first shown
	plans    map[string][]string  // a steps card's labels, as first shown, by id
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
// messages, then each card's title, text, button words, note and option
// labels.
func (s *said) texts() []string {
	out := append([]string(nil), s.messages...)
	for _, id := range s.order {
		c := s.cards[id]
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
	}
	return out
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
// quote the chat (a word count, the slug wanted), so a public log prints them.
var counted = map[string]bool{"brief": true, "names the agent from the taf's name": true,
	"names no planque": true, "keeps its plan": true}

// gradeSetup grades Setup's conversation. Each check is a name, whether it
// passed, and why when it didn't.
// modes is what the grants this run connected say about test or live
// (kiff/tafs#89): the scripted owner chooses test, and the fake vendor's
// sign-in is in test mode.
type modes struct {
	live []string // tools pinned to live mode
	test bool     // a tool pinned to test mode
	read bool     // Setup read what the sign-in covers (planque describe)
}

// gradeSetup grades what Setup wrote, and the modes it pinned.
func gradeSetup(ctx context.Context, j *eval.Judge, o owner, s said, tafAgent string, m modes) ([]eval.CheckResult, error) {
	var out []eval.CheckResult
	add := func(name string, g eval.Grade) { out = append(out, eval.CheckResult{Name: name, Grade: g}) }

	// 1. The owner's language, in everything Setup wrote.
	g, err := j.Ask(ctx, fmt.Sprintf("Every numbered text below is written in %s. Proper names, product and company names, ids, "+
		"URLs, amounts and tool names don't count against it, nor does the word taf (tafs' word for an agent, in every language).", o.language), numbered(s.texts()))
	if err != nil {
		return nil, err
	}
	add("in the owner's language", g)

	// 2. Brief: no message over maxWords words.
	var long []string
	for i, m := range s.messages {
		if n := len(strings.Fields(m)); n > maxWords {
			long = append(long, fmt.Sprintf("message %d has %d words", i+1, n))
		}
	}
	if len(long) > 0 {
		add("brief", eval.Grade{Why: strings.Join(long, "; ") + fmt.Sprintf(" (at most %d)", maxWords)})
	} else {
		add("brief", eval.Grade{Pass: true})
	}

	// 3. It never asks the owner for an id, a key or JSON: in a message or in
	// a component (a confirm can ask too).
	g, err = j.Ask(ctx, "None of these texts (messages, and the titles, texts and options of what was shown) asks the owner to give, "+
		"type or choose an id, an identifier, an agent name for a system, a key, a token or JSON. Naming one the agent chose itself is fine.",
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
	g, err = j.Ask(ctx, "No message asks the owner to choose between fixed options by listing them in prose for the owner to "+
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
	// Or it read the mode from the vendor and pinned it (kiff/tafs#90): the
	// vendor knows what the owner signed in to, so it needn't ask.
	if asked || (m.read && m.test) {
		add("asks test or live", eval.Grade{Pass: true})
	} else {
		add("asks test or live", eval.Grade{Why: "no choice between test and live mode was shown to tap, and no mode the vendor reported was pinned"})
	}
	if len(m.live) > 0 {
		add("pins the mode the owner chose", eval.Grade{Why: "pinned to live mode, though the owner chose test: " + strings.Join(m.live, ", ")})
	} else {
		add("pins the mode the owner chose", eval.Grade{Pass: true})
	}
	return out, nil
}

// What tells a test-or-live choice's options apart, in the owners'
// languages. Test is read first: "Test mode (no real money)" is test.
var (
	testWords = []string{"test", "prueba"}
	liveWords = []string{"live", "real", "producción", "en vivo"}
)

// modeChoice returns a confirm's test option, when it offers test and live.
func modeChoice(c component) (test string, ok bool) {
	if c.Type != "confirm" {
		return "", false
	}
	live := false
	for _, o := range c.Options {
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
		if c.Type != "confirm" {
			continue
		}
		var labels []string
		for _, o := range c.Options {
			labels = append(labels, o.Label)
		}
		out = append(out, strings.TrimSpace(c.Text+" ["+strings.Join(labels, " | ")+"]"))
	}
	return out
}
