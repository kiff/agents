package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/kiff/agents/eval"
)

// rulesAsData is the gate on the rules as data (draft-the-card@26): the
// rules notice the owner agreed to carries its rules beside its words, and
// the confirm after it names it, so tafs cloud holds the Card to exactly
// what was agreed. While tafs cloud only logs a draft with no rules, a
// setup that sends none would pass every other gate, so this one fails it.
const rulesAsData = "the agreed rules are sent as data"

// draftStaysInside is the gate on a draft that reaches no one (-invoices):
// its profile says outside: false, so it carries no disclosure, and the taf
// doesn't disclose it. A draft that doesn't say is counted as reaching
// outside, which is the safe side, so a missing mark fails here, not there.
const draftStaysInside = "a draft that stays inside says so"

// gradeRulesAsData grades rulesAsData from what Setup showed and the tools
// the run connected. Skipped with no write tool connected, or no rules
// notice shown at all: signsWhatWasAgreed says so then.
func gradeRulesAsData(s said) eval.CheckResult {
	result := func(g eval.Grade) eval.CheckResult { return eval.CheckResult{Name: rulesAsData, Grade: g} }
	var writes []grantTool
	for _, tl := range s.connected {
		if !tl.ReadOnly && !tl.SetupReads {
			writes = append(writes, tl)
		}
	}
	if len(writes) == 0 {
		return result(eval.Grade{Skipped: true, Why: "no write tool connected"})
	}
	notice, next, ok := agreedNotice(s)
	if !ok {
		return result(eval.Grade{Skipped: true, Why: "no rules notice shown before a confirm"})
	}
	var problems []string
	if len(notice.Rules) == 0 {
		problems = append(problems, "the agreed notice carries no rules, only words")
	}
	ruled := map[string]shownRule{}
	for _, r := range notice.Rules {
		ruled[foldTool(r.Tool)] = r
	}
	for _, tl := range writes {
		r, found := ruled[foldTool(tl.Tool)]
		if !found {
			r, found = ruled[foldTool(tl.Name)]
		}
		name := tl.Name
		if name == "" {
			name = tl.Tool
		}
		switch {
		case len(notice.Rules) == 0:
		case !found:
			problems = append(problems, name+" has no rule")
		case r.Over != "ask" && r.Over != "refuse":
			problems = append(problems, fmt.Sprintf("%s's rule says over %q, not ask or refuse", name, r.Over))
		case tl.AmountArg != "" && len(r.Limits) == 0:
			problems = append(problems, name+" moves money and its rule has no figures")
		}
		// A figure may say its own over ("ask past €5 a refund, refuse past €50 a day").
		for _, l := range r.Limits {
			if found && l.Over != "" && l.Over != "ask" && l.Over != "refuse" {
				problems = append(problems, fmt.Sprintf("%s's %s per %s says over %q, not ask or refuse", name, l.Quantity, l.Window, l.Over))
			}
		}
	}
	switch {
	case next.Type != "confirm":
		problems = append(problems, "the card after the agreed notice is "+next.Type+", not a confirm that agrees to it")
	case next.Agrees == "":
		problems = append(problems, "the confirm after the agreed notice doesn't name it (agrees)")
	case next.Agrees != notice.ID:
		problems = append(problems, fmt.Sprintf("the confirm agrees to %q, not to the notice shown before it", next.Agrees))
	case !slicesContainsValue(next.Options, "agree"):
		problems = append(problems, "the confirm has no option with value agree")
	}
	if len(problems) > 0 {
		return result(eval.Grade{Why: strings.Join(problems, "; ")})
	}
	return result(eval.Grade{Pass: true})
}

// figuresUnsaid is each figure on the agreed notice's rules that its words
// don't say, in the Card's units or in major units (a hundredth): advice,
// since formats vary by language, as tafs cloud only logs it.
func figuresUnsaid(s said) eval.CheckResult {
	result := func(g eval.Grade) eval.CheckResult {
		return eval.CheckResult{Name: "the agreed figures are in the notice's words", Advisory: true, Grade: g}
	}
	notice, _, ok := agreedNotice(s)
	if !ok || len(notice.Rules) == 0 {
		return result(eval.Grade{Skipped: true, Why: "no rules as data to compare"})
	}
	said := numbersIn(notice.Title + "\n" + notice.Text)
	var unsaid []string
	for _, r := range notice.Rules {
		for _, l := range r.Limits {
			if l.Limit == nil {
				continue
			}
			v := float64(*l.Limit)
			if !said[v] && !said[v/100] {
				unsaid = append(unsaid, fmt.Sprintf("%s %s per %s %d", r.Tool, l.Quantity, l.Window, *l.Limit))
			}
		}
	}
	if len(unsaid) > 0 {
		return result(eval.Grade{Why: "the notice's words don't say " + strings.Join(unsaid, ", ")})
	}
	return result(eval.Grade{Pass: true})
}

// agreedNotice is the last rules notice Setup put to the owner, and the card
// shown after it: the rules tafs cloud holds the Card to.
func agreedNotice(s said) (notice, next component, ok bool) {
	for i, id := range s.order {
		c, found := s.cards[id]
		if !found || c.Type != "notice" || i+1 >= len(s.order) || !agreesTo(s.cards[s.order[i+1]]) {
			continue
		}
		notice, next, ok = c, s.cards[s.order[i+1]], true
	}
	return notice, next, ok
}

// gradeDraftStaysInside grades draftStaysInside: every draft operation
// (none_until_approved) in the connected profiles says outside: false, and
// the taf's result doesn't disclose its tool. Skipped when no connected
// profile has one, as on a key request, which carries no profile. Run on -invoices only, whose
// drafts are invoices: a draft email or reply elsewhere is counted as
// reaching outside, and undisclosed checks that.
func gradeDraftStaysInside(s said) eval.CheckResult {
	result := func(g eval.Grade) eval.CheckResult { return eval.CheckResult{Name: draftStaysInside, Grade: g} }
	type op struct {
		Tool     string `json:"tool"`
		Effect   string `json:"effect"`
		Resource *struct {
			Outside *bool `json:"outside"`
		} `json:"resource"`
	}
	var drafts []op
	for _, raw := range s.profiles {
		var p struct {
			Operations []op `json:"operations"`
		}
		if json.Unmarshal(raw, &p) != nil {
			continue
		}
		for _, o := range p.Operations {
			if o.Effect == "none_until_approved" {
				drafts = append(drafts, o)
			}
		}
	}
	if len(drafts) == 0 {
		// A key request's tools carry no profile, so there is nothing to mark and
		// nothing tafs cloud checks: skipped, as "narrows a managed sign-in" is with no
		// managed app. A draft's disclosure there rests on Coach's line alone, until
		// the key path carries a profile.
		return result(eval.Grade{Skipped: true, Why: "no connected profile has a draft operation (none_until_approved): a key request carries no profile, so nothing checks its drafts here"})
	}
	// Every tool the taf's latest result discloses, with fields or not.
	disclosed := map[string]bool{}
	for _, id := range s.order {
		if c, found := s.cards[id]; found && c.Type == "result" && c.Taf != nil {
			disclosed = map[string]bool{}
			for _, d := range c.Taf.Disclose {
				disclosed[foldTool(d.Tool)] = true
			}
		}
	}
	var problems []string
	for _, d := range drafts {
		switch {
		case d.Resource == nil || d.Resource.Outside == nil:
			problems = append(problems, d.Tool+" doesn't say outside: false, so it carries the disclosure")
		case *d.Resource.Outside:
			problems = append(problems, d.Tool+" says outside: true")
		}
		if disclosed[foldTool(d.Tool)] || disclosed[foldTool(kiffName(s, d.Tool))] {
			problems = append(problems, d.Tool+" is in the taf's disclose")
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return result(eval.Grade{Why: strings.Join(problems, "; ")})
	}
	return result(eval.Grade{Pass: true})
}

// kiffName is a connected tool's name in KIFF, from its name at the vendor.
func kiffName(s said, tool string) string {
	for _, tl := range s.connected {
		if foldTool(tl.Tool) == foldTool(tool) && tl.Name != "" {
			return tl.Name
		}
	}
	return tool
}

func foldTool(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

func slicesContainsValue(options []option, value string) bool {
	for _, o := range options {
		if o.Value == value {
			return true
		}
	}
	return false
}

// numberIn matches a number as a text writes it, with spaces, tabs, dots, commas
// or apostrophes between its digits. A number never runs across a line break:
// "€5\n50 a day" is 5 and 50, not 550.
var numberIn = regexp.MustCompile(`\d(?:[\d \t.,\x{00a0}\x{202f}']*\d)?`)

// numbersIn is every number the text writes, read each way its separators
// allow: the last , or . as the decimal mark, or every one between thousands.
func numbersIn(text string) map[float64]bool {
	out := map[float64]bool{}
	for _, m := range numberIn.FindAllString(text, -1) {
		raw := strings.NewReplacer(" ", "", "\t", "", " ", "", " ", "", "'", "").Replace(m)
		add := func(v string) {
			if n, err := strconv.ParseFloat(v, 64); err == nil {
				out[n] = true
			}
		}
		if last := strings.LastIndexAny(raw, ".,"); last >= 0 {
			add(strings.NewReplacer(".", "", ",", "").Replace(raw[:last]) + "." + raw[last+1:])
		}
		add(strings.NewReplacer(".", "", ",", "").Replace(raw))
	}
	return out
}
