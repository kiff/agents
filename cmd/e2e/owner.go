package main

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/kiff/agents/cmd/internal/testaccount"
)

// The test account's client, phone, grants and sweep are shared with the
// Eval (cmd/internal/testaccount); these are their names here.
type (
	client    = testaccount.Client
	answer    = testaccount.Answer
	phone     = testaccount.Phone
	grant     = testaccount.Grant
	grantTool = testaccount.GrantTool
	grantArg  = testaccount.GrantArgument
)

// rawWords is what the owner shouldn't read in a grant's approval text
// (kiff/tafs#138, items 52, 53): a tool with no title from Setup, a tool's
// own name (a grant prefix such as bxkwf7vi_refund_payment), or the
// vendor's host outside the line that says where to sign in, or two tools
// with one title. Empty when the text is in words.
func rawWords(g grant) string {
	var words []string
	for _, line := range strings.Split(g.Text, "\n") {
		if !strings.HasPrefix(line, "You will sign in at ") {
			words = append(words, line)
		}
	}
	text := strings.Join(words, "\n")
	titled := map[string]bool{}
	for _, t := range g.Request.Tools {
		title := strings.ToLower(strings.TrimSpace(t.Title))
		switch {
		case title != "" && titled[title]:
			return fmt.Sprintf("two tools titled %q", title)
		case strings.TrimSpace(t.Title) == "":
			return "Setup gave " + t.Tool + " no title"
		case t.Name != "" && strings.Contains(text, t.Name):
			return "the tool's name " + t.Name
		}
		titled[title] = true
	}
	// An API-key grant's own words say where the key goes, host and all:
	// planque's, not Setup's.
	if u, err := url.Parse(g.Request.Server); err == nil && u.Host != "" && g.Request.Kind != "api_key" && strings.Contains(text, u.Host) {
		return "the vendor's host " + u.Host
	}
	return ""
}
