package show

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Wording is a rule a definition sets on what its cards may say, as data: a
// card whose own words match one of Patterns is refused with Fix, so the agent
// redraws it before anyone sees it, and it's never recorded. Each pattern is
// RE2, matched without case on whole words (an edge is anything but a letter
// or a digit, in any script, so "só lê" has edges as "read-only" does). A
// pattern's first group, when it has one, is the word after the phrase: when
// it's one of UnlessNext, that match names what the agent's job does ("it
// reads only the new rows"), not what a sign-in allows, and is let through.
type Wording struct {
	Patterns   []string `json:"patterns"`
	UnlessNext []string `json:"unless_next,omitempty"`
	Fix        string   `json:"fix"`
}

// Limits on a definition's wording rules.
const (
	MaxWordingRules    = 32
	MaxWordingPatterns = 32
	MaxWordingPattern  = 500
	MaxWordingFix      = 500
)

// CheckWording says what's wrong with a definition's wording rules, or nil.
func CheckWording(rules []Wording) error {
	if len(rules) > MaxWordingRules {
		return fmt.Errorf("wording: at most %d rules", MaxWordingRules)
	}
	for i, w := range rules {
		if len(w.Patterns) == 0 || len(w.Patterns) > MaxWordingPatterns {
			return fmt.Errorf("wording %d: 1 to %d patterns", i, MaxWordingPatterns)
		}
		for _, p := range w.Patterns {
			if strings.TrimSpace(p) == "" || len(p) > MaxWordingPattern {
				return fmt.Errorf("wording %d: a pattern is empty or longer than %d bytes", i, MaxWordingPattern)
			}
			if _, err := compiled(p); err != nil {
				return fmt.Errorf("wording %d: %v", i, err)
			}
		}
		if strings.TrimSpace(w.Fix) == "" || utf8.RuneCountInString(w.Fix) > MaxWordingFix {
			return fmt.Errorf("wording %d: fix is 1 to %d characters", i, MaxWordingFix)
		}
	}
	return nil
}

// WithWording returns t refusing cards that break rules. Rules are checked
// when the definition is (CheckWording); a pattern that won't compile here
// matches nothing.
func (t Tool) WithWording(rules []Wording) Tool {
	t.wording = rules
	return t
}

var patterns sync.Map // pattern → *regexp.Regexp

// compiled is p on whole words, without case, compiled once.
func compiled(p string) (*regexp.Regexp, error) {
	if re, ok := patterns.Load(p); ok {
		return re.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(`(?i)(?:^|[^\pL\pN])(?:` + p + `)(?:$|[^\pL\pN])`)
	if err != nil {
		return nil, err
	}
	patterns.Store(p, re)
	return re, nil
}

// Claim is the first match in text of one of w's patterns that isn't let
// through by UnlessNext, lower-cased and trimmed to the phrase itself (never
// the words around it, so it's safe in a public log); "" when there's none.
// The Eval's gate reads the same rules through it.
func Claim(text string, w Wording) string {
	for _, p := range w.Patterns {
		re, err := compiled(p)
		if err != nil {
			continue
		}
		// One match at a time: a match takes its edges with it, so after one that's let through, the
		// scan goes on from where its next word starts, or "read only the read-only tab" would hide
		// "read-only" behind the space "read only the" used up (qa-kiff on agents#551).
		for at := 0; at <= len(text); {
			loc := re.FindStringSubmatchIndex(text[at:])
			if loc == nil {
				break
			}
			group := ""
			if len(loc) > 3 && loc[2] >= 0 {
				group = text[at+loc[2] : at+loc[3]]
			}
			if next := strings.TrimSpace(group); next != "" && unless(next, w.UnlessNext) {
				// From the next word's edge on, never inside a word (^ would make a start there).
				step := loc[1]
				if r, _ := utf8.DecodeRuneInString(group); edge(r) && loc[2] > 0 {
					step = loc[2]
				}
				at += max(step, 1)
				continue
			}
			phrase := text[at+loc[0] : at+loc[1]]
			if group != "" {
				phrase = strings.TrimSuffix(strings.TrimRightFunc(phrase, edge), group)
			}
			return strings.ToLower(strings.TrimFunc(phrase, edge))
		}
	}
	return ""
}

// edge is a character that isn't part of a word: what bounds a match.
func edge(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }

func unless(next string, words []string) bool {
	for _, u := range words {
		if strings.EqualFold(next, strings.TrimSpace(u)) {
			return true
		}
	}
	return false
}

// worded is the first rule c's own words break, as an error naming where, the
// rule's phrase and its fix; "" when none.
func worded(c Component, rules []Wording) string {
	if len(rules) == 0 {
		return ""
	}
	for _, f := range cardTexts(c) {
		for _, w := range rules {
			if p := Claim(f.text, w); p != "" {
				return fmt.Sprintf("the card's %s says %q: %s", f.where, p, w.Fix)
			}
		}
	}
	return ""
}

type cardText struct{ where, text string }

// cardTexts are what a card says in its own words: its title, text, buttons
// and note, its steps, its options and its questions with theirs. A card's
// other fields (the taf it sets up, a tool's name, a grant's id) aren't checked.
func cardTexts(c Component) []cardText {
	var out []cardText
	add := func(where, s string) {
		if strings.TrimSpace(s) != "" {
			out = append(out, cardText{where, s})
		}
	}
	add("title", c.Title)
	add("text", c.Text)
	add("yes", c.Yes)
	add("no", c.No)
	add("note", c.Note)
	for _, s := range c.Steps {
		add("step", s.Label)
	}
	for _, o := range c.Options {
		add("option", o.Label)
		add("option", o.Description)
	}
	for _, q := range c.Questions {
		add("question", q.Text)
		add("question", q.Note)
		for _, o := range q.Options {
			add("option", o.Label)
			add("option", o.Description)
		}
	}
	return out
}
