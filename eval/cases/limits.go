package cases

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/kiff/agents/eval"
)

// LimitsRead is the check that a limit the report names is one this run read
// (kiff/agents#599): every amount written beside a limit word ("the $100
// limit", "a limit of €50") is in what the run read of its limits, KIFF's
// Card (kiff_card), KIFF's answer on a call it held or refused, or what the
// owner told the taf (its note). A limit stated from memory, or guessed, can
// be out of date once the owner changes the Card, and a person reading the
// report can't tell it from one that was checked (Eval 44469206833,
// Chargebacks run 3: "$150 is above the $100 limit for one close call", in a
// run that never tried the close nor read its Card).
//
// It backs up the judged "its words don't contradict the record" in code. It
// reads KIFF's two ways of writing a limit: formatted ("$45 is over the $40
// limit for one call") and in the Card's minor units ("sum(amount) of 15000
// is above the per-action limit of 10000").
const LimitsRead = "names a limit only from what it read"

// limitWord is a word that makes an amount beside it a limit, in the owner's
// languages.
var limitWord = regexp.MustCompile(`(?i)^(limits?|caps?|capped|maximum|max|límites?|tope|máximo|limites?|plafond|grenze|höchstbetrag|höchstens|massimo|limiet|maximaal|maksymalnie|limitu)$`)

// amount is an amount of money as a report writes it: a sign or a code
// before or after its figure.
var amount = regexp.MustCompile(`[$€£]\s?\d(?:[\d.,']*\d)?|\d(?:[\d.,']*\d)?\s?(?:€|£|\$|\b(?:EUR|USD|GBP|CHF)\b)`)

// figure is a number in what the run read: a formatted amount's, or a count
// of minor units.
var figure = regexp.MustCompile(`\d(?:[\d.,']*\d)?`)

// Before and after are how many words from a limit word an amount may be to
// be its figure: "the $100 limit", "a limit of 100 EUR". Further off, it's
// another amount in the sentence ("$150 is above the $100 limit": $150 is the
// dispute's).
const (
	limitBefore = 2
	limitAfter  = 3
)

func (s Scenario) limitsRead(_ context.Context, f facts, _ *eval.Judge) (eval.Grade, error) {
	if strings.TrimSpace(f.report) == "" {
		return eval.Grade{Skipped: true, Why: "the run wrote no report"}, nil
	}
	named := namedLimits(f.report)
	if len(named) == 0 {
		return eval.Grade{Pass: true}, nil
	}
	read := limitSources(f, s.Said)
	for _, n := range named {
		if !readAmount(n, read) {
			return eval.Grade{Why: fmt.Sprintf("the report names %s as a limit, and nothing this run read says it: no kiff_card result, no answer from KIFF on a call, and not the owner's note", n)}, nil
		}
	}
	return eval.Grade{Pass: true}, nil
}

// namedLimits is each amount the report writes beside a limit word.
func namedLimits(report string) []string {
	words := strings.Fields(report)
	var out []string
	for i, w := range words {
		if !limitWord.MatchString(strings.Trim(w, ".,;:!?()\"'«»“”")) {
			continue
		}
		before := strings.Join(words[max(0, i-limitBefore):i], " ")
		after := strings.Join(words[i+1:min(len(words), i+1+limitAfter)], " ")
		out = append(out, amount.FindAllString(before, -1)...)
		out = append(out, amount.FindAllString(after, -1)...)
	}
	return out
}

// limitSources is what the run read of its limits: its Card reads, KIFF's
// answers on its calls (an error result, or a recorded reason a call wasn't
// done), any result that names a limit, and what the owner said.
func limitSources(f facts, said []string) []string {
	out := append([]string(nil), said...)
	for _, r := range f.results {
		if strings.HasSuffix(r.Tool, "kiff_card") || strings.HasSuffix(r.Tool, "kiff_pending") || r.IsError || strings.Contains(strings.ToLower(r.Text), "limit") {
			out = append(out, r.Text)
		}
	}
	for _, c := range f.called {
		if c.Reason != "" {
			out = append(out, c.Reason)
		}
	}
	return out
}

// readAmount reports whether the amount named is in what was read: the same
// figure, formatted, or as a count of minor units (a hundredth).
func readAmount(named string, read []string) bool {
	v, ok := value(figure.FindString(named))
	if !ok {
		return true // nothing to compare: never a fail on a figure it can't read
	}
	for _, text := range read {
		for _, n := range figure.FindAllString(text, -1) {
			got, ok := value(n)
			if !ok {
				continue
			}
			if same(got, v) || (!strings.ContainsAny(n, ".,'") && same(got, v*100)) {
				return true
			}
		}
	}
	return false
}

// value reads a figure written either way: "1,234.50", "1.234,50", "1'234",
// "15000". A separator followed by exactly two digits at the end is the
// decimal point; any other is a thousands separator.
func value(s string) (float64, bool) {
	s = strings.ReplaceAll(s, "'", "")
	if s == "" {
		return 0, false
	}
	dec := ""
	if i := strings.LastIndexAny(s, ".,"); i >= 0 && len(s)-i-1 == 2 {
		s, dec = s[:i], s[i+1:]
	}
	s = strings.NewReplacer(".", "", ",", "").Replace(s)
	if dec != "" {
		s += "." + dec
	}
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil
}

func same(a, b float64) bool { return math.Abs(a-b) < 0.005 }
