package show

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// legal-i18n's endings, as tafs's Setup definition sends them.
var keptWordsForTest = KeptWords{
	"en": {"connection": "(the connection keeps it)", "kiff": "(KIFF adds it)", "instructions": "(in its instructions)"},
	"fr": {"connection": "(la connexion s'en charge)", "kiff": "(KIFF l'ajoute)", "instructions": "(dans ses instructions)"},
}

const lineRules = `"rules":[{"tool":"append_row","over":"refuse","limits":[{"quantity":"count","window":"calendar_day","limit":50}]},{"tool":"read_rows","over":"refuse"}]`

func shown(t *testing.T, input string) (Component, string) {
	t.Helper()
	r, err := New().WithKeptWords(keptWordsForTest).Call(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	if r.IsError {
		return Component{}, r.Text
	}
	var c Component
	if err := json.Unmarshal(r.Component, &c); err != nil {
		t.Fatalf("%v: %s", err, r.Text)
	}
	return c, ""
}

// A rules notice's lines are sent with where each is kept, never its ending:
// show writes the text, each line then the definition's ending for its kept in
// the notice's language, and a Card's line with none. The stored component
// carries the lines and the text it drew.
func TestARulesNoticesTextIsWrittenFromItsLines(t *testing.T) {
	c, why := shown(t, `{"type":"notice","id":"rules","tone":"info","title":"Its rules","lang":"en",`+lineRules+`,"lines":[`+
		`{"text":"Adds at most 50 rows a day","kept":"card","tools":["append_row"]},`+
		`{"text":"Writes only to the Leads sheet","kept":"connection","tools":["append_row"]},`+
		`{"text":"Ends each message with its AI disclosure","kept":"kiff","what":"disclosure"},`+
		`{"text":"Skips anyone it already wrote to","kept":"instructions"}]}`)
	if why != "" {
		t.Fatal(why)
	}
	want := "Adds at most 50 rows a day\nWrites only to the Leads sheet (the connection keeps it)\nEnds each message with its AI disclosure (KIFF adds it)\nSkips anyone it already wrote to (in its instructions)"
	if c.Text != want {
		t.Fatalf("text:\n%s\nwant\n%s", c.Text, want)
	}
	if len(c.Lines) != 4 || c.Lines[1].Kept != KeptConnection || c.Lang != "en" {
		t.Fatalf("the lines aren't kept beside the text: %+v", c)
	}
	// In French, French endings.
	c, why = shown(t, `{"type":"notice","id":"rules","tone":"info","lang":"fr",`+lineRules+`,"lines":[{"text":"Saute qui a déjà reçu un message","kept":"instructions"}]}`)
	if why != "" || c.Text != "Saute qui a déjà reçu un message (dans ses instructions)" {
		t.Fatalf("%q %s", c.Text, why)
	}
}

func TestARulesNoticesLinesAreRefusedUnlessEachSaysWhereItsKept(t *testing.T) {
	for input, want := range map[string]string{
		`"lang":"en","lines":[{"text":"Skips anyone","kept":""}]`:                                 "kept is one of card, connection, kiff, instructions",
		`"lang":"en","lines":[{"text":"Skips anyone","kept":"memory"}]`:                           "kept is one of",
		`"lang":"en","text":"Its rules","lines":[{"text":"Skips anyone","kept":"instructions"}]`:  "no text of its own",
		`"lang":"de","lines":[{"text":"Skips anyone","kept":"instructions"}]`:                     "lang is the owner's language, one of en, fr",
		`"lines":[{"text":"Skips anyone","kept":"instructions"}]`:                                 "lang is the owner's language",
		`"lang":"en","lines":[{"text":"Writes only to Leads","kept":"connection"}]`:               "a connection line names its tools",
		`"lang":"en","lines":[{"text":"Sends mail","kept":"connection","tools":["send_mail"]}]`:   `"send_mail" isn't a tool of this notice's rules`,
		`"lang":"en","lines":[{"text":"At most 9 sends","kept":"card","tools":["send_mail"]}]`:    `"send_mail" isn't a tool of this notice's rules`,
		`"lang":"en","lines":[{"text":"Asks you past it","kept":"kiff"}]`:                         "KIFF keeps only disclosure, reads_total, held_approval",
		`"lang":"en","lines":[{"text":"Refunds","kept":"kiff","what":"refunds"}]`:                 "KIFF keeps only",
		`"lang":"en","lines":[{"text":"Skips anyone","kept":"instructions","what":"disclosure"}]`: "what is for a kiff line",
		// An ending typed in a line's words, in any language, is refused: show adds it.
		`"lang":"en","lines":[{"text":"Skips anyone (in its instructions)","kept":"card","tools":["append_row"]}]`: `leave out "(in its instructions)"`,
		`"lang":"en","lines":[{"text":"Skips anyone (dans ses instructions)","kept":"instructions"}]`:              `leave out "(dans ses instructions)"`,
		// Plain words on one line: no markup, no break.
		`"lang":"en","lines":[{"text":"<b>Skips</b> anyone","kept":"instructions"}]`: "plain words on one line",
		`"lang":"en","lines":[{"text":"Skips\nanyone","kept":"instructions"}]`:       "plain words on one line",
		`"lang":"en","lines":[{"text":"Skips [anyone](x)","kept":"instructions"}]`:   "plain words on one line",
		`"lang":"en","lines":[{"text":"  ","kept":"instructions"}]`:                  "text is 1 to 300 characters",
	} {
		_, why := shown(t, `{"type":"notice","id":"rules","tone":"info",`+lineRules+`,`+input+`}`)
		if !strings.Contains(why, want) {
			t.Errorf("%s: %q, want %q", input, why, want)
		}
	}
	// Too many lines.
	lines := strings.Repeat(`{"text":"Skips anyone","kept":"instructions"},`, MaxLines+1)
	if _, why := shown(t, `{"type":"notice","id":"rules","tone":"info","lang":"en",`+lineRules+`,"lines":[`+strings.TrimSuffix(lines, ",")+`]}`); !strings.Contains(why, "at most 32") {
		t.Errorf("33 lines: %q", why)
	}
	// Lines go on a rules notice.
	if _, why := shown(t, `{"type":"notice","id":"n","tone":"info","lang":"en","lines":[{"text":"Skips anyone","kept":"instructions"}]}`); !strings.Contains(why, "lines go on a rules notice") {
		t.Errorf("lines with no rules: %q", why)
	}
}

// Until TextOnlyUntil a rules notice may still bring its own text and no
// lines, as before; from then, show refuses it. This test fails once the date
// has passed, so the rollover can't quietly stay: end it, or move the date
// with a reason.
func TestARulesNoticeWithoutLinesIsRefusedOnceTheRolloverEnds(t *testing.T) {
	if !time.Now().Before(TextOnlyUntil) {
		t.Fatalf("the rollover ended on %s: refuse rules notices without lines for good (drop TextOnlyUntil)", TextOnlyUntil.Format(time.DateOnly))
	}
	plain := `{"type":"notice","id":"rules","tone":"info","text":"Adds at most 50 rows a day",` + lineRules + `}`
	if _, why := shown(t, plain); why != "" {
		t.Fatalf("during the rollover: %s", why)
	}
	defer func(was func() time.Time) { now = was }(now)
	now = func() time.Time { return TextOnlyUntil }
	if _, why := shown(t, plain); !strings.Contains(why, "a rules notice sends lines") {
		t.Fatalf("after the rollover: %q", why)
	}
	// A notice with no rules is a plain notice, as ever.
	if _, why := shown(t, `{"type":"notice","id":"n","tone":"info","text":"Connected."}`); why != "" {
		t.Fatal(why)
	}
}

func TestADefinitionsKeptWordsHaveEveryEndingInEachLanguage(t *testing.T) {
	if err := CheckKeptWords(keptWordsForTest); err != nil {
		t.Fatal(err)
	}
	for name, kw := range map[string]KeptWords{
		"a missing ending":   {"de": {"connection": "(die Verbindung sorgt dafür)", "kiff": "(KIFF fügt das hinzu)"}},
		"an empty ending":    {"de": {"connection": "(x)", "kiff": " ", "instructions": "(x)"}},
		"a card ending":      {"de": {"connection": "(x)", "kiff": "(x)", "instructions": "(x)", "card": "(x)"}},
		"not a code":         {"deu": {"connection": "(x)", "kiff": "(x)", "instructions": "(x)"}},
		"two lines":          {"de": {"connection": "(x)\n(y)", "kiff": "(x)", "instructions": "(x)"}},
		"too long an ending": {"de": {"connection": "(" + strings.Repeat("x", MaxKeptWords) + ")", "kiff": "(x)", "instructions": "(x)"}},
	} {
		if CheckKeptWords(kw) == nil {
			t.Errorf("%s was taken", name)
		}
	}
}

// A definition with no kept words (one written before them, until its next
// Upgrade) is never offered lines or lang, and a notice sent with them anyway
// is refused with a fix that names the form it takes: its own text, as
// before lines. The text form still shows, rules and all.
func TestADefinitionWithoutKeptWordsTakesTheNoticesOwnText(t *testing.T) {
	plain := New()
	schema := string(plain.Spec().InputSchema)
	if strings.Contains(schema, `"lines"`) || strings.Contains(schema, `"lang"`) {
		t.Fatal("lines or lang offered to a definition with no kept words")
	}
	if !json.Valid(plain.Spec().InputSchema) {
		t.Fatal("the schema without lines isn't JSON")
	}
	with := New().WithKeptWords(keptWordsForTest)
	if s := string(with.Spec().InputSchema); !strings.Contains(s, `"lines"`) || !strings.Contains(s, `"lang"`) || !json.Valid(with.Spec().InputSchema) {
		t.Fatal("lines and lang not offered with kept words")
	}
	call := func(input string) (string, bool) {
		r, err := plain.Call(context.Background(), json.RawMessage(input))
		if err != nil {
			t.Fatal(err)
		}
		return r.Text, r.IsError
	}
	why, refused := call(`{"type":"notice","id":"rules","tone":"info","lang":"en",` + lineRules + `,"lines":[{"text":"Skips anyone","kept":"instructions"}]}`)
	if !refused || !strings.Contains(why, "send its text and its rules, with no lines and no lang") {
		t.Fatalf("lines without kept words: %v %q", refused, why)
	}
	if why, refused := call(`{"type":"notice","id":"rules","tone":"info","text":"Adds at most 50 rows a day",` + lineRules + `}`); refused {
		t.Fatalf("the text form, refused: %q", why)
	}
}
