package show

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// A rules notice says, line by line, where each rule is kept: on the Card
// (KeptCard), by the connection (KeptConnection), by KIFF (KeptKIFF), or only
// in the taf's instructions (KeptInstructions). The agent sends the lines with
// their Kept, never the ending: show writes the notice's text from them, each
// line's words then the definition's ending for its Kept in the notice's
// language (KeptWords), so an ending can't be dropped or reworded, and what
// each line claims is data the checks after it read. A Card's line has no
// ending.
const (
	KeptCard         = "card"
	KeptConnection   = "connection"
	KeptKIFF         = "kiff"
	KeptInstructions = "instructions"
)

// Kepts are the places a line may say it's kept, KeptCard first.
var Kepts = []string{KeptCard, KeptConnection, KeptKIFF, KeptInstructions}

// KIFFHolds are the only things a KeptKIFF line may say KIFF does, by its
// What: KIFF writes a taf's AI disclosure into what it sends outside
// (disclosure), counts the reads every Card carries (reads_total), and holds a
// call past its limit for the owner (held_approval). "KIFF adds it" is the
// strongest of the four claims, so it is only ever one of these.
var KIFFHolds = []string{"disclosure", "reads_total", "held_approval"}

// Line is one line of a rules notice: its words, where it's kept, the tools
// it's about (a Card's or a connection's line: tools the notice's rules name),
// and, for KIFF's, which of KIFFHolds it is.
type Line struct {
	Text  string   `json:"text"`
	Kept  string   `json:"kept"`
	Tools []string `json:"tools,omitempty"`
	What  string   `json:"what,omitempty"`
}

// KeptWords are a definition's endings for a rules notice's lines: by
// language (ISO 639-1), the words for each Kept but KeptCard, such as
// {"en": {"connection": "(the connection keeps it)", "kiff": "(KIFF adds it)",
// "instructions": "(in its instructions)"}}.
type KeptWords map[string]map[string]string

// Limits on a notice's lines and a definition's endings.
const (
	MaxLines     = 32
	MaxLine      = 300
	MaxKeptWords = 80
)

// TextOnlyUntil ends the rollover: until then a rules notice may still carry
// its own text and no lines, as before lines, and is graded as it was. From
// then, show refuses it. A test fails once it has passed, so it can't stay.
var TextOnlyUntil = time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC)

// now is show's clock, for the rollover.
var now = time.Now

var langCode = regexp.MustCompile(`^[a-z]{2}$`)

// CheckKeptWords says what's wrong with a definition's endings, or nil: each
// language has a two-letter code and all three endings, each 1 to
// MaxKeptWords characters, on one line.
func CheckKeptWords(kw KeptWords) error {
	for lang, words := range kw {
		if !langCode.MatchString(lang) {
			return fmt.Errorf("kept_words: %q isn't a two-letter language code", lang)
		}
		for _, k := range Kepts[1:] {
			w := strings.TrimSpace(words[k])
			if w == "" || utf8.RuneCountInString(w) > MaxKeptWords || strings.ContainsAny(w, "\n\r") {
				return fmt.Errorf("kept_words %s: %q is 1 to %d characters on one line", lang, k, MaxKeptWords)
			}
		}
		for k := range words {
			if !slices.Contains(Kepts[1:], k) {
				return fmt.Errorf("kept_words %s: %q isn't connection, kiff or instructions", lang, k)
			}
		}
	}
	return nil
}

// WithKeptWords returns t writing a rules notice's lines with kw's endings.
func (t Tool) WithKeptWords(kw KeptWords) Tool {
	t.keptWords = kw
	return t
}

// markupLike is a line that carries markup or a line break: a line is drawn as
// plain text, one line, and never read as anything else.
var markupLike = regexp.MustCompile(`[<>\x60\n\r]|\]\(|\*\*`)

// drawLines checks a rules notice's lines and writes its text from them, or
// says what to fix. A notice with no lines is as before until TextOnlyUntil.
func (t Tool) drawLines(c *Component, ownText bool) error {
	if c.Type != "notice" {
		return nil
	}
	if len(c.Lines) == 0 {
		if len(c.Rules) > 0 && !now().Before(TextOnlyUntil) {
			return fmt.Errorf("a rules notice sends lines: [{text, kept: card|connection|kiff|instructions, tools?, what?}] and lang, and no text: show writes the text")
		}
		if c.Lang != "" {
			return fmt.Errorf("lang goes with lines")
		}
		return nil
	}
	// A definition with no endings (written before them, until its next
	// Upgrade) takes a rules notice's own text, as before lines: the fix says
	// so, so the notice is sent again in that form, never left unshown.
	if len(t.keptWords) == 0 {
		return fmt.Errorf("this Coach writes a rules notice's own text: send its text and its rules, with no lines and no lang")
	}
	if ownText {
		return fmt.Errorf("a notice with lines has no text of its own: show writes it from the lines, with each line's ending")
	}
	if len(c.Rules) == 0 {
		return fmt.Errorf("lines go on a rules notice, with its rules")
	}
	if len(c.Lines) > MaxLines {
		return fmt.Errorf("lines: at most %d", MaxLines)
	}
	words, ok := t.keptWords[c.Lang]
	if !ok {
		return fmt.Errorf("lang is the owner's language, one of %s", strings.Join(t.langs(), ", "))
	}
	ruled := make([]string, 0, len(c.Rules))
	for _, r := range c.Rules {
		ruled = append(ruled, r.Tool)
	}
	text := make([]string, 0, len(c.Lines))
	for i, l := range c.Lines {
		n := utf8.RuneCountInString(strings.TrimSpace(l.Text))
		if n == 0 || n > MaxLine {
			return fmt.Errorf("line %d: text is 1 to %d characters", i+1, MaxLine)
		}
		if markupLike.MatchString(l.Text) {
			return fmt.Errorf("line %d: plain words on one line, no markup", i+1)
		}
		if e := t.typedEnding(l.Text); e != "" {
			return fmt.Errorf("line %d: leave out %q: show adds each line's ending from its kept", i+1, e)
		}
		switch l.Kept {
		case KeptCard, KeptConnection:
			if len(l.Tools) == 0 {
				return fmt.Errorf("line %d: a %s line names its tools", i+1, l.Kept)
			}
			for _, tl := range l.Tools {
				if !slices.Contains(ruled, tl) {
					return fmt.Errorf("line %d: %q isn't a tool of this notice's rules", i+1, tl)
				}
			}
		case KeptKIFF:
			if !slices.Contains(KIFFHolds, l.What) {
				return fmt.Errorf("line %d: KIFF keeps only %s (what); anything else is the Card's, the connection's or in its instructions", i+1, strings.Join(KIFFHolds, ", "))
			}
		case KeptInstructions:
		default:
			return fmt.Errorf("line %d: kept is one of %s", i+1, strings.Join(Kepts, ", "))
		}
		if l.Kept != KeptKIFF && l.What != "" {
			return fmt.Errorf("line %d: what is for a kiff line", i+1)
		}
		line := strings.TrimSpace(l.Text)
		if l.Kept != KeptCard {
			line += " " + words[l.Kept]
		}
		text = append(text, line)
	}
	c.Text = strings.Join(text, "\n")
	if utf8.RuneCountInString(c.Text) > MaxText {
		return fmt.Errorf("the lines come to more than %d characters: fewer or shorter lines", MaxText)
	}
	return nil
}

// typedEnding is an ending of any language that text already carries, or "".
func (t Tool) typedEnding(text string) string {
	lower := strings.ToLower(text)
	for _, lang := range t.langs() {
		for _, k := range Kepts[1:] {
			if e := t.keptWords[lang][k]; e != "" && strings.Contains(lower, strings.ToLower(e)) {
				return e
			}
		}
	}
	return ""
}

// langs are the languages t has endings for, sorted.
func (t Tool) langs() []string {
	langs := make([]string, 0, len(t.keptWords))
	for l := range t.keptWords {
		langs = append(langs, l)
	}
	slices.Sort(langs)
	return langs
}
