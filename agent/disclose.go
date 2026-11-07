package agent

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Disclosure is the line every call that writes to someone outside the
// company carries first, saying an AI agent wrote it, and for whom. The
// runtime puts it in the call, so it never depends on the model remembering
// (Eval 42193895936: a dispute's evidence went to the card network without
// it).
type Disclosure struct {
	// Line is the sentence as sent: "Written by an AI agent for the company."
	Line string `json:"line"`
	// Lines, when set, is the same sentence in other languages, by ISO 639-1
	// code ("es": "Escrito por un agente de IA para la empresa."): the
	// disclosure goes in the language of the text it heads when one of these
	// is in it, and as Line otherwise.
	Lines map[string]string `json:"lines,omitempty"`
	// In is where it goes: for each tool that writes outside, the free-text
	// fields of its input, by dotted path. Each one a call carries as text
	// starts with it; the first is where it goes alone when a call carries
	// none of them.
	In []DisclosureIn `json:"in"`
	// After is what a credit's line followed (registry Definition.Credit):
	// checked and kept for a definition that still names one. Nothing is put
	// after the disclosure any more.
	After string `json:"after,omitempty"`
}

// DisclosureIn is one tool's place for the line: Tool by its name, with or
// without the part KIFF adds to it for a connection; Fields by dotted path.
type DisclosureIn struct {
	Tool   string   `json:"tool"`
	Fields []string `json:"fields"`
}

// MaxDisclosureLine bounds the line: one sentence.
const MaxDisclosureLine = 300

// MaxDisclosureLines bounds Lines: a few languages, not a dictionary.
const MaxDisclosureLines = 12

// languageCode is an ISO 639-1 code, as Lines keys it.
var languageCode = regexp.MustCompile(`^[a-z]{2}$`)

// Check reports what's wrong with d, if anything.
func (d *Disclosure) Check() error {
	if d == nil {
		return nil
	}
	if strings.TrimSpace(d.Line) == "" || len(d.Line) > MaxDisclosureLine || strings.ContainsAny(d.Line, "\n\r") {
		return fmt.Errorf("a disclosure's line is one sentence of 1 to %d bytes", MaxDisclosureLine)
	}
	if d.After != "" && (len(d.After) > MaxDisclosureLine || strings.ContainsAny(d.After, "\n\r")) {
		return fmt.Errorf("a disclosure's after is one sentence of at most %d bytes", MaxDisclosureLine)
	}
	if len(d.Lines) > MaxDisclosureLines {
		return fmt.Errorf("a disclosure has its line in at most %d other languages", MaxDisclosureLines)
	}
	for code, l := range d.Lines {
		if !languageCode.MatchString(code) {
			return fmt.Errorf("a disclosure's lines are keyed by a two-letter language code, not %q", code)
		}
		if strings.TrimSpace(l) == "" || len(l) > MaxDisclosureLine || strings.ContainsAny(l, "\n\r") {
			return fmt.Errorf("a disclosure's line in %s is one sentence of 1 to %d bytes", code, MaxDisclosureLine)
		}
	}
	if len(d.In) == 0 {
		return errors.New("a disclosure names the tools it goes in")
	}
	for _, in := range d.In {
		if in.Tool == "" || len(in.Fields) == 0 {
			return errors.New("a disclosure's tool has a name and at least one field")
		}
		for _, f := range in.Fields {
			for _, part := range strings.Split(f, ".") {
				if part == "" {
					return fmt.Errorf("a disclosure's field is a dotted path, not %q", f)
				}
			}
		}
	}
	return nil
}

// fields is where the line goes in a call of tool, or nil when it goes in
// none: a tool named as given, or with one word of KIFF's for a connection
// after it or a vendor's before it ("update_dispute_k3f9",
// "stripe_update_dispute"); never another tool whose name only starts the
// same ("send_test_mode_off" for "send", agentgabo on #352).
func (d *Disclosure) fields(tool string) []string {
	if d == nil {
		return nil
	}
	for _, in := range d.In {
		after, isAfter := strings.CutPrefix(tool, in.Tool+"_")
		before, isBefore := strings.CutSuffix(tool, "_"+in.Tool)
		if tool == in.Tool || isAfter && word(after) || isBefore && word(before) {
			return in.Fields
		}
	}
	return nil
}

// word is one lowercase word of letters and digits, at most 16: a
// connection's part or a vendor's name, never more of a tool's name.
func word(s string) bool {
	if s == "" || len(s) > 16 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// textField is one of a call's fields that carries text: its parent object and its key.
type textField struct {
	parent map[string]any
	key    string
}

func (f textField) text() string { return f.parent[f.key].(string) }

// html says whether the field is a message's html part: its name says so
// ("html", "body_html", "htmlBody"), or its text is a whole html document.
func (f textField) html() bool {
	if strings.Contains(strings.ToLower(f.key), "html") {
		return true
	}
	t := strings.ToLower(strings.TrimLeft(f.text(), space))
	return strings.HasPrefix(t, "<!doctype") || strings.HasPrefix(t, "<html")
}

// First is input as the call sends it, with the line first in each of the
// tool's fields the call carries as text, or as the first field when the call
// carries its parent and none of them. That's the AI disclosure's place:
// whoever reads the message, in whichever of its parts they're shown (a text
// body or its html), reads first that an AI agent wrote it, and for whom (AI
// Act Art. 50(1); the Commission's guidelines show the label at the top). The
// line is in the message's language when Lines has it, read from all the
// fields together, so its parts say the same line. It is said once in each
// field: one the model wrote anywhere in it, in any language, is taken out
// first. Unchanged when that changes nothing, when the call carries none of
// the fields nor the first's parent, and for any other tool.
func (d *Disclosure) First(tool string, input json.RawMessage) json.RawMessage {
	paths := d.fields(tool)
	if len(paths) == 0 {
		return input
	}
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber() // an amount stays as the model wrote it
	var call map[string]any
	if dec.Decode(&call) != nil || call == nil {
		return input
	}
	var texts []textField // the tool's fields the call carries as text, in order
	for _, path := range paths {
		parent, last := parentOf(call, path)
		if parent == nil {
			continue
		}
		if _, ok := parent[last].(string); ok {
			texts = append(texts, textField{parent, last})
		}
	}
	changed := false
	if len(texts) == 0 {
		parent, last := parentOf(call, paths[0])
		if parent == nil {
			return input
		}
		parent[last], changed = d.Line, true
	}
	lines := d.lines()
	rest := make([]string, len(texts)) // each field's text without the line
	for i, f := range texts {
		rest[i] = strings.TrimLeft(without(f.text(), lines), space)
	}
	line := d.lineFor(strings.Join(rest, "\n\n"))
	for i, f := range texts {
		next := line
		switch {
		case f.html():
			next = withHTMLLine(rest[i], line)
		case strings.TrimSpace(rest[i]) != "":
			next += "\n\n" + rest[i]
		}
		if next != f.text() {
			f.parent[f.key], changed = next, true
		}
	}
	if !changed {
		return input
	}
	// As the model wrote it: no HTML escapes, which change the text sent but not its value (agentgabo on #352).
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if enc.Encode(call) != nil {
		return input
	}
	return bytes.TrimRight(out.Bytes(), "\n")
}

// lines is the line in every language it has, each also as html writes it
// ("&amp;" for "&") and as an html part's paragraph of its own (htmlLine),
// longest first so none is taken out as part of another.
func (d *Disclosure) lines() []string {
	var out []string
	for _, l := range append([]string{d.Line}, slices.Collect(maps.Values(d.Lines))...) {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		out = append(out, l, html.EscapeString(l), "<p>"+l+"</p>", htmlLine(l))
	}
	slices.SortFunc(out, func(a, b string) int { return cmp.Or(len(b)-len(a), strings.Compare(a, b)) })
	return slices.Compact(out)
}

// lineFor is the line for a text: in its language when Lines has it, Line otherwise.
func (d *Disclosure) lineFor(text string) string {
	if l, ok := d.Lines[language(text)]; ok {
		return l
	}
	return d.Line
}

// space is what separates a line from the text around it.
const space = " \t\r\n"

// htmlLine is the line as an html part shows it: a paragraph of its own,
// escaped, so it never runs into the message's first sentence.
func htmlLine(line string) string { return "<p>" + html.EscapeString(line) + "</p>" }

// beforeContent are the tags the line goes just after in an html part, in
// order of preference: inside its body; else after its head, its html tag or
// its doctype. Text before a doctype would put a browser in quirks mode.
var beforeContent = []*regexp.Regexp{
	regexp.MustCompile(`(?i)<body\b[^>]*>`),
	regexp.MustCompile(`(?i)</head\s*>`),
	regexp.MustCompile(`(?i)<html\b[^>]*>`),
	regexp.MustCompile(`(?i)^<!doctype[^>]*>`),
}

// withHTMLLine is an html part with the line first in what it shows.
func withHTMLLine(text, line string) string {
	p := htmlLine(line)
	for _, re := range beforeContent {
		if at := re.FindStringIndex(text); at != nil {
			return text[:at[1]] + p + text[at[1]:]
		}
	}
	return p + text
}

// without is text with each of lines taken out wherever it is, with the space
// around it: one that starts or ends the text takes the space after or before
// it, and one between two runs of space leaves the longer. One in a quote of
// an earlier message stays (quoted). The rest of the text is as it was, so a
// text with none of them is unchanged.
func without(text string, lines []string) string {
	for _, l := range lines {
		if l == "" {
			continue
		}
		for from := 0; ; {
			j := strings.Index(text[from:], l)
			if j < 0 {
				break
			}
			i := from + j
			if quoted(text, i) {
				from = i + len(l)
				continue
			}
			from = 0
			before, after := text[:i], text[i+len(l):]
			head, tail := strings.TrimRight(before, space), strings.TrimLeft(after, space)
			switch gap, gapAfter := before[len(head):], after[:len(after)-len(tail)]; {
			case head == "":
				text = tail
			case tail == "":
				text = head
			default:
				if len(gapAfter) > len(gap) {
					gap = gapAfter
				}
				text = head + gap + tail
			}
		}
	}
	return text
}

// quoted says whether the text at i is in a quote of an earlier message: on a
// line that starts with ">", or inside an html blockquote. A quote is the
// earlier message as it was sent, its line included.
func quoted(text string, i int) bool {
	start := strings.LastIndexByte(text[:i], '\n') + 1
	if strings.HasPrefix(strings.TrimLeft(text[start:i], " \t"), ">") {
		return true
	}
	lower := strings.ToLower(text[:i])
	return strings.LastIndex(lower, "<blockquote") > strings.LastIndex(lower, "</blockquote")
}

// commonWords are frequent short words of each language whose line a
// disclosure may have, enough to tell which one a message is written in.
var commonWords = map[string]map[string]bool{
	"en": wordSet("the and is are you your to of for with we our this that it was has have been will be not on at by from thanks thank order please"),
	"es": wordSet("el la los las del de que y en por para con una un su sus tu tus te le es está están hemos ha han gracias pedido hola usted ya pero muy más"),
	"pt": wordSet("o os as do da dos das de que e em por para com uma um seu sua você não é está foi obrigado obrigada pedido olá já mas muito mais"),
	"fr": wordSet("le la les des du de et est pour pas vous nous une un que dans avec sur votre vos merci commande été sont bonjour déjà mais très plus"),
	"de": wordSet("der die das und ist nicht sie ihr ihre ein eine mit für auf den dem des zu wir danke bestellung wurde bitte hallo schon aber sehr mehr"),
	"it": wordSet("il lo gli della del di che e è per non una un con sono grazie ordine suo sua tuo tua ciao già ma molto più stato"),
}

// markup is an html tag, which language reads past.
var markup = regexp.MustCompile(`<[^<>]*>`)

// languageOrder is the order languages are compared in, so a tie is decided the same way every time.
var languageOrder = []string{"en", "es", "pt", "fr", "de", "it"}

func wordSet(words string) map[string]bool {
	set := map[string]bool{}
	for _, w := range strings.Fields(words) {
		set[w] = true
	}
	return set
}

// language is the language text is written in, by ISO 639-1 code, when its
// common words say so clearly: at least two of them, and more than any other
// language's. Empty otherwise: a short or mixed text gets the default line.
// It only picks which of a disclosure's lines heads the text.
func language(text string) string {
	counts := map[string]int{}
	text = markup.ReplaceAllString(text, " ") // an html part's tags aren't its words
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) }) {
		for _, lang := range languageOrder {
			if commonWords[lang][w] {
				counts[lang]++
			}
		}
	}
	best, top, second := "", 0, 0
	for _, lang := range languageOrder {
		switch n := counts[lang]; {
		case n > top:
			best, top, second = lang, n, top
		case n > second:
			second = n
		}
	}
	if top < 2 || top == second {
		return ""
	}
	return best
}

// parentOf is the object holding path's last part in call, and that part;
// nil when an object on the way isn't there.
func parentOf(call map[string]any, path string) (map[string]any, string) {
	parts := strings.Split(path, ".")
	at := call
	for _, p := range parts[:len(parts)-1] {
		next, ok := at[p].(map[string]any)
		if !ok {
			return nil, ""
		}
		at = next
	}
	return at, parts[len(parts)-1]
}
