package anthropic

import (
	"regexp"
	"strings"
)

// A reply's text can carry a tag of the model's own format, leaked into the
// words meant for the owner: "antml:final_answer_dialog Falta que apruebes…"
// reached a chat in Eval 42141761407 (the PM, qa-kiff). Every reply text is
// cleaned of them here, the one place the stream, the stored message and what
// tafs builds from it all read from. Only the model's reply text is: never an
// owner's words, a tool's result or a thinking block, which goes back signed.

// leakedTag is one tag, whole: bracketed, opening or closing, the prefix and a
// name with anything up to its ">"; or bare, with any emphasis around
// it and one space after it ("antml:final_answer_dialog ", "**antml:foo** ").
var leakedTag = regexp.MustCompile(`(?i)</?antml:[a-z_][a-z0-9_]*[^<>]*>|[*_]*\bantml:[a-z_][a-z0-9_]*\b[*_]*[ \t]?`)

// stripTags is s without the leaked tags in it.
func stripTags(s string) string {
	if !strings.Contains(strings.ToLower(s), "antml:") {
		return s
	}
	return leakedTag.ReplaceAllString(s, "")
}

// maybeTag is a text's end that could be the start of a tag still coming: a
// "<" not yet closed that could open one ("<", "</a", "<fin"), or the
// bare word's start ("a", "ant", "antml:final_"), with any emphasis before it.
var maybeTag = regexp.MustCompile(`(?i)(?:</?(?:a(?:n(?:t(?:m(?:l(?::[a-z0-9_]*[^<>]*)?)?)?)?)?)?|[*_]*\ba(?:n(?:t(?:m(?:l(?::[a-z0-9_]*[*_]*)?)?)?)?)?)$`)

// tagFilter passes a reply's streamed text on without the leaked tags, which
// can come split across deltas ("antml:final_" then "answer_dialog"): it holds
// back an end that could be a tag's start until the next delta settles it,
// and flush sends what's left once the reply ends.
type tagFilter struct {
	out     func(string)
	pending string
}

func (f *tagFilter) write(delta string) {
	s := f.pending + delta
	hold := len(s)
	if loc := maybeTag.FindStringIndex(s); loc != nil {
		hold = loc[0]
	}
	f.pending = s[hold:]
	if text := stripTags(s[:hold]); text != "" {
		f.out(text)
	}
}

func (f *tagFilter) flush() {
	if text := stripTags(f.pending); text != "" {
		f.out(text)
	}
	f.pending = ""
}
