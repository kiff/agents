package main

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// The scripted owners of Hindi, Urdu and Turkmen (the refunds setup alone): what each says is in its language and
// script, carries the same two limits as the others' (a refund over 5 euros waits for the owner's phone, 50 euros a day)
// and the agreement, names the taf in its own script, and gives the vendor it is asked about. The judge is told the
// language by its name, and the setup's checks that read a notice's language read the code the owner has.
func TestTheScriptedOwnersOfTheNewerLanguages(t *testing.T) {
	for _, c := range []struct {
		code, language string
		script         *unicode.RangeTable
		taf            string
	}{
		{"hi", "Hindi", unicode.Devanagari, "रिफंड"},
		{"ur", "Urdu", unicode.Arabic, "ریفنڈ"},
		{"tk", "Turkmen", unicode.Latin, "Yzyna töleg"},
	} {
		o, ok := owners[c.code]
		if !ok {
			t.Errorf("no scripted owner for %s", c.code)
			continue
		}
		if o.lang != c.code || o.language != c.language || o.taf != c.taf {
			t.Errorf("%s: lang %q language %q taf %q", c.code, o.lang, o.language, o.taf)
		}
		ask := o.ask("https://vendor.example/mcp")
		if !strings.Contains(ask, "https://vendor.example/mcp") || !strings.Contains(ask, "MCP") {
			t.Errorf("%s asks nothing about the vendor: %q", c.code, ask)
		}
		if len(o.facts) != 2 {
			t.Fatalf("%s: %d answers", c.code, len(o.facts))
		}
		for _, n := range []string{"5", "50"} { // each as a number of its own: "50" doesn't say "5"
			if !regexp.MustCompile(`(^|[^0-9])` + n + `([^0-9]|$)`).MatchString(o.facts[0]) {
				t.Errorf("%s: the limits are 5 euros a refund and 50 a day, no %s in %q", c.code, n, o.facts[0])
			}
		}
		// What the owner writes is mostly in the language's script (the vendor, MCP, taf and numbers are not).
		for _, text := range []string{ask, o.facts[0], o.facts[1]} {
			letters, own := 0, 0
			for _, r := range text {
				if unicode.IsLetter(r) {
					letters++
					if unicode.Is(c.script, r) {
						own++
					}
				}
			}
			if own*2 <= letters {
				t.Errorf("%s: %q isn't mostly %s", c.code, text, c.language)
			}
		}
		// Hindi and Urdu are not the Latin-script language: the script tells them from Turkmen and from each other.
		other := map[string]*unicode.RangeTable{"hi": unicode.Arabic, "ur": unicode.Devanagari, "tk": unicode.Devanagari}[c.code]
		for _, r := range ask + o.facts[0] {
			if unicode.Is(other, r) {
				t.Errorf("%s: %q has a letter of another script", c.code, string(r))
				break
			}
		}
		if o.deskAsk != nil || o.invoiceAsk != nil || !refundsOnly[c.code] {
			t.Errorf("%s: the refunds setup alone", c.code)
		}
	}
	for _, code := range []string{"en", "es"} {
		if refundsOnly[code] {
			t.Errorf("%s has every setup", code)
		}
	}
}

// A refunds-only owner can't be asked for the desk, billing, improve, a recipe, the people or a replay (they have no
// words in those), and an unknown owner is told which languages there are.
func TestARefundsOnlyOwnerHasTheRefundsSetupAlone(t *testing.T) {
	none := func(string) string { return "" }
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-owner", "hi", "-desk"}, "refunds setup alone"},
		{[]string{"-owner", "ur", "-invoices"}, "refunds setup alone"},
		{[]string{"-owner", "tk", "-improve"}, "refunds setup alone"},
		{[]string{"-owner", "hi", "-people"}, "refunds setup alone"},
		{[]string{"-owner", "xx"}, "hi, ur or tk"},
	} {
		err := run(context.Background(), c.args, &redacting{w: &bytes.Buffer{}}, none)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want an error saying %q", c.args, err, c.want)
		}
	}
}

// Setup's test-or-live choice is told in these languages too: the option that says test, whatever its value.
func TestATestOrLiveChoiceIsToldInTheNewerLanguages(t *testing.T) {
	for lang, options := range map[string][2]string{
		"hi": {"टेस्ट मोड (असली पैसा नहीं)", "लाइव मोड"},
		"ur": {"ٹیسٹ موڈ (حقیقی پیسہ نہیں)", "لائیو موڈ"},
		"tk": {"Synag režimi (hakyky pul däl)", "Janly režim"},
	} {
		test, ok := modeOptions([]option{{Value: "a", Label: options[0]}, {Value: "b", Label: options[1]}})
		if !ok || test != "a" {
			t.Errorf("%s: test option %q, offers both %v", lang, test, ok)
		}
		// Only one of the two: no choice between them.
		if _, ok := modeOptions([]option{{Value: "a", Label: options[0]}}); ok {
			t.Errorf("%s: one option is no choice", lang)
		}
	}
}
