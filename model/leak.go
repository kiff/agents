package model

import (
	"regexp"
	"strings"
)

// A tag of the model's own format can leak into text meant for the owner:
// "antml:final_answer_dialog Falta que apruebes…" reached a chat in Eval
// 42141761407. StripLeakedTags is that text without them, for every place the
// model writes words someone reads: a reply's text, a progress note, a card
// it shows. Only text the model wrote: never an owner's words or a tool's
// result.

// LeakedTag is one tag, whole: bracketed, opening or closing, the prefix and a
// name with anything up to its ">"; or bare, with any emphasis around it and
// one space after it ("antml:final_answer_dialog ", "**antml:foo** "). A name
// may hold a hyphen ("antml:final-answer", qa-kiff).
var LeakedTag = regexp.MustCompile(`(?i)</?antml:[a-z_][a-z0-9_-]*[^<>]*>|[*_]*\bantml:[a-z_][a-z0-9_-]*\b[*_]*[ \t]?`)

// StripLeakedTags is s without the leaked tags in it.
func StripLeakedTags(s string) string {
	if !strings.Contains(strings.ToLower(s), "antml:") {
		return s
	}
	return LeakedTag.ReplaceAllString(s, "")
}
