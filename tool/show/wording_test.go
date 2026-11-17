package show_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kiff/agents/tool/show"
)

// A definition's rules, as tafs sends Coach's: en's read-only phrases with the
// job's exception, and pt's, whose edges aren't ASCII.
var readOnly = []show.Wording{
	{
		Patterns:   []string{`read-only|read only(\s+\pL+)?`, `(?:changes?|modif(?:y|ies)|touch(?:es)?) nothing`},
		UnlessNext: []string{"the", "a", "an", "its", "your", "new", "each", "every", "rows", "emails"},
		Fix:        "never call a connection, a sign-in or a step read-only; say what the job does",
	},
	{
		Patterns:   []string{`(?:só|apenas|somente) (?:de )?leitura`, `(?:só|apenas|somente) lê(\s+\pL+)?`, `lê (?:só|apenas|somente)(\s+\pL+)?`},
		UnlessNext: []string{"as", "os", "suas", "seus", "linhas"},
		Fix:        "nunca diga que uma ligação é só de leitura",
	},
}

func wordingCall(t *testing.T, rules []show.Wording, input string) (bool, string) {
	t.Helper()
	r, err := show.New().WithWording(rules).Call(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	// A refused card carries no component, so the run records none and no app draws it.
	if r.IsError && r.Component != nil {
		t.Fatalf("%s: refused, but its component is still there to record", input)
	}
	return r.IsError, r.Text
}

// Every place a card says something in its own words is checked, and a card
// that breaks a rule is refused with the rule's fix, so it's never recorded.
func TestACardThatBreaksAWordingRuleIsRefusedWithItsFix(t *testing.T) {
	for _, in := range []string{
		`{"type":"steps","id":"p1","steps":[{"label":"Connect Google Sheets, read-only","status":"active"}]}`,
		`{"type":"notice","id":"n1","tone":"info","title":"Read-only access","text":"Sheets"}`,
		`{"type":"notice","id":"n2","tone":"info","text":"The sign-in is READ ONLY."}`,
		`{"type":"notice","id":"n3","tone":"info","text":"Connecting it changes nothing."}`,
		`{"type":"confirm","id":"c1","text":"Connect?","options":[{"value":"yes","label":"Yes, read-only"},{"value":"no","label":"No"}]}`,
		`{"type":"questions","id":"q4","text":"Before I connect:","questions":[{"id":"a","text":"Connect?","options":[{"value":"y","label":"Yes","description":"it modifies nothing"},{"value":"n","label":"No"}]}]}`,
		`{"type":"questions","id":"q1","text":"Before I connect:","questions":[{"id":"a","text":"Connect Sheets read-only?","options":[{"value":"y","label":"Yes"},{"value":"n","label":"No"}]}]}`,
		`{"type":"questions","id":"q2","text":"Before I connect:","questions":[{"id":"a","text":"Which sheet?","note":"it's read-only","options":[{"value":"y","label":"Yes"},{"value":"n","label":"No"}]}]}`,
		`{"type":"questions","id":"q3","text":"Before I connect:","questions":[{"id":"a","text":"Which?","options":[{"value":"y","label":"Somente leitura"},{"value":"n","label":"No"}]}]}`,
		`{"type":"sign_in","id":"s1","grant":"grt_AbC-1","service":"sheets.example","yes":"Sign in, read only","no":"Not now"}`,
		`{"type":"sign_in","id":"s2","grant":"grt_AbC-1","service":"sheets.example","yes":"Sign in","no":"Not now","note":"A ligação só lê."}`,
		`{"type":"notice","id":"n4","tone":"info","text":"It can read only what it needs."}`,
	} {
		refused, text := wordingCall(t, readOnly, in)
		if !refused || !strings.Contains(text, "the card's") || !(strings.Contains(text, "never call") || strings.Contains(text, "nunca diga")) {
			t.Errorf("%s\nshown: %v %q", in, !refused, text)
		}
	}
}

// What the job does is said freely: "read only" followed by what it reads, a
// "nothing changes" that isn't a claim, and words inside other words.
func TestWhatTheJobDoesIsShown(t *testing.T) {
	for _, in := range []string{
		`{"type":"steps","id":"p1","steps":[{"label":"Read only the new rows","status":"active"}]}`,
		`{"type":"notice","id":"n1","tone":"info","text":"It will read only the rows added since its last run."}`,
		`{"type":"notice","id":"n2","tone":"info","text":"If you decline, nothing changes."}`,
		`{"type":"notice","id":"n3","tone":"info","text":"It commonly reads your sheet; the Card limits it."}`,
		`{"type":"notice","id":"n4","tone":"info","text":"A taf lê só as linhas novas."}`,
		`{"type":"notice","id":"n5","tone":"info","text":"Thread-only reading of the sheet."}`,
	} {
		if refused, text := wordingCall(t, readOnly, in); refused {
			t.Errorf("%s\nrefused: %q", in, text)
		}
	}
}

// The job's exception is the word right after: "lê apenas o que precisa" is a
// claim, since "o" isn't one of the words that name what's read.
func TestTheExceptionIsOnlyTheNextWord(t *testing.T) {
	if refused, _ := wordingCall(t, readOnly, `{"type":"notice","id":"n1","tone":"info","text":"A ligação lê apenas o que precisa."}`); !refused {
		t.Error(`"lê apenas o que" was shown`)
	}
	if got := show.Claim("Sim, só lê.", readOnly[1]); got != "lê só" && got != "só lê" && got != "" {
		t.Errorf("Claim = %q", got)
	}
	if got := show.Claim("Connect it, read-only.", readOnly[0]); got != "read-only" {
		t.Errorf("Claim = %q, want the phrase alone", got)
	}
	if got := show.Claim("It reads only what it needs: read only what", readOnly[0]); got != "read only" {
		t.Errorf("Claim = %q, want the phrase without the next word", got)
	}
}

// With no rules, every card is as it was.
func TestNoWordingRulesChangeNothing(t *testing.T) {
	if refused, text := wordingCall(t, nil, `{"type":"notice","id":"n1","tone":"info","text":"Read-only, it changes nothing."}`); refused {
		t.Errorf("refused with no rules: %q", text)
	}
}

// A definition's rules are bounded and must compile.
func TestWordingRulesAreChecked(t *testing.T) {
	ok := []show.Wording{{Patterns: []string{"read-only"}, Fix: "say what the job does"}}
	if err := show.CheckWording(ok); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]show.Wording{
		{{Patterns: nil, Fix: "x"}},
		{{Patterns: []string{"("}, Fix: "x"}},
		{{Patterns: []string{" "}, Fix: "x"}},
		{{Patterns: []string{"read-only"}, Fix: ""}},
		{{Patterns: []string{"read-only"}, Fix: strings.Repeat("x", show.MaxWordingFix+1)}},
	} {
		if show.CheckWording(bad) == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// qa-kiff on agents#551: a match let through by its next word takes its edge
// with it, and the claim right after it is still found.
func TestAClaimRightAfterALetThroughIsFound(t *testing.T) {
	w := show.Wording{Patterns: []string{`read-only`, `read only(\s+\pL+)?`}, UnlessNext: []string{"the", "rows"}, Fix: "say what the job does"}
	for text, want := range map[string]string{
		"It will read only the read-only tab.":                      "read-only",
		"It reads only the rows; the sign-in is read-only.":         "read-only",
		"It will read only the rows, then read only what it needs.": "read only",
		"It will read only the rows added since its last run.":      "",
	} {
		if got := show.Claim(text, w); got != want {
			t.Errorf("%q: %q, want %q", text, got, want)
		}
	}
}
