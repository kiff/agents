package main

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/kiff/agents/eval"
)

// The bar for connecting an app (13 Nov): what every setup is held to, counted
// from what the run did, never voted on.
//
//  1. One approval per app, plus its Card: the owner approves an app once, and
//     its Card once (or both in one approval).
//  2. No raw error reaches the person: nothing Setup says or shows carries a
//     connection server's, KIFF's or a vendor's own words. Read from the run
//     itself: six words in a row from any error the run got, so the gate
//     follows each source's wording as it changes. A short list of wrappers
//     and codes is the backstop for what never comes as an error.
//  3. No loop: a refused request or tools call is never sent again as it
//     was (its pins included), and nothing already approved is asked for
//     again.
const (
	oneApprovalPerApp  = "one approval per app"
	noRawErrorShown    = "no raw error reaches the person"
	noLoopAfterRefusal = "no loop"
)

// appOf names the app a request is for, so requests and approvals for the
// same app count together whatever the grant: its catalog entry, else its MCP
// server's host, else where its description is kept (its host and folder: two
// APIs of one vendor, described apart, are two apps). A reuse names no app of
// its own.
func appOf(a connectAsk) string {
	switch {
	case a.Entry != "":
		return "entry " + a.Entry
	case a.Server != "":
		return "server " + hostOf(a.Server)
	case a.APIURL != "":
		return "api " + folderOf(a.APIURL)
	}
	return ""
}

// folderOf is a URL's host and the folder its file is in, in lower case:
// https://v.example/mail/openapi.json is v.example/mail.
func folderOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	dir := u.Path
	if i := strings.LastIndex(dir, "/"); i >= 0 {
		dir = dir[:i]
	}
	return strings.ToLower(u.Host + dir)
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return strings.ToLower(u.Host)
	}
	return raw
}

// barRun is what the bar is counted from: Setup's planque requests and tools
// calls, the approvals the owner answered, what Setup stated, and the text of
// every error the run got.
type barRun struct {
	asks, toolsAsks []connectAsk
	approvals       []approvalSeen
	statements      []string
	errors          []string
}

// gradeConnectBar grades the bar's three gates over the run. Each is skipped,
// with why, when the run didn't do what it grades.
func gradeConnectBar(r barRun) []eval.CheckResult {
	asks, approvals := r.asks, r.approvals
	var out []eval.CheckResult
	gate := func(name string, g eval.Grade) { out = append(out, eval.CheckResult{Name: name, Grade: g}) }

	// The app each grant is for, from the request that made it. A reused
	// grant keeps the app of the request that made it.
	appOfGrant := map[string]string{}
	for _, a := range asks {
		if a.Grant != "" && appOf(a) != "" {
			appOfGrant[a.Grant] = appOf(a)
		}
	}
	for _, a := range asks {
		if a.Grant != "" && appOfGrant[a.Grant] == "" && a.Reuse != "" {
			appOfGrant[a.Grant] = appOfGrant[a.Reuse]
		}
	}

	// 1. One approval per app, plus its Card. An approval that carries a
	// draft is the Card's (alone, or with the app's): one of those per app,
	// and one without.
	if len(approvals) == 0 {
		gate(oneApprovalPerApp, eval.Grade{Skipped: true, Why: "no approval was asked for"})
	} else {
		type count struct{ app, card int }
		per := map[string]*count{}
		var order []string
		for _, ap := range approvals {
			if ap.Grant == "" || ap.Again {
				continue
			}
			app := appOfGrant[ap.Grant]
			if app == "" {
				app = "grant " + ap.Grant
			}
			c, ok := per[app]
			if !ok {
				c = &count{}
				per[app] = c
				order = append(order, app)
			}
			if ap.Draft != "" {
				c.card++
			} else {
				c.app++
			}
		}
		var over []string
		for _, app := range order {
			c := per[app]
			if c.app > 1 || c.card > 1 {
				over = append(over, fmt.Sprintf("%s: %d approval(s) for the app and %d for its Card", app, c.app, c.card))
			}
		}
		if len(over) > 0 {
			gate(oneApprovalPerApp, eval.Grade{Why: strings.Join(over, "; ")})
		} else {
			gate(oneApprovalPerApp, eval.Grade{Pass: true})
		}
	}

	// 2. No raw error reaches the person.
	var shown []string
	quoted := sixWordsOf(r.errors)
	for _, s := range r.statements {
		if hit := quotedError(s, quoted); hit != "" {
			shown = append(shown, fmt.Sprintf("an error's words %q in %q", hit, oneLineOf(s)))
		} else if hit := rawError(s); hit != "" {
			shown = append(shown, fmt.Sprintf("%q in %q", hit, oneLineOf(s)))
		}
	}
	if len(shown) > 0 {
		gate(noRawErrorShown, eval.Grade{Why: strings.Join(shown, "; ")})
	} else {
		gate(noRawErrorShown, eval.Grade{Pass: true})
	}

	// 3. No loop: a request planque refused, sent again the same way; or an
	// approval for a grant the owner already approved, shown again.
	var loops []string
	refused := map[string]string{}
	for _, a := range asks {
		k := askShape(a)
		if why, ok := refused[k]; ok {
			loops = append(loops, fmt.Sprintf("%s asked again as it was after %q", appOrReuse(a), why))
		}
		if a.Refused != "" {
			refused[k] = a.Refused
		}
	}
	for _, a := range r.toolsAsks {
		k := "tools|" + a.Grant + "|" + a.Tools
		if why, ok := refused[k]; ok {
			loops = append(loops, fmt.Sprintf("tools for %s sent again as they were after %q", a.Grant, why))
		}
		if a.Refused != "" {
			refused[k] = a.Refused
		}
	}
	approved := map[string]int{}
	for _, ap := range approvals {
		if ap.Grant == "" {
			continue
		}
		if ap.Again {
			loops = append(loops, fmt.Sprintf("approval for %s shown again once it was approved", ap.Grant))
			continue
		}
		k := ap.Grant
		if ap.Draft != "" {
			k += " with its Card"
		}
		approved[k]++
	}
	var again []string
	for k, n := range approved {
		if n > 1 {
			again = append(again, fmt.Sprintf("approval for %s shown %d times", k, n))
		}
	}
	sort.Strings(again)
	loops = append(loops, again...)
	switch {
	case len(asks) == 0 && len(r.toolsAsks) == 0 && len(approvals) == 0:
		gate(noLoopAfterRefusal, eval.Grade{Skipped: true, Why: "no connection was asked for or approved"})
	case len(loops) > 0:
		gate(noLoopAfterRefusal, eval.Grade{Why: strings.Join(loops, "; ")})
	default:
		gate(noLoopAfterRefusal, eval.Grade{Pass: true})
	}
	return out
}

// askShape is a request as sent: its app (or the grant it reuses), its uses,
// its tools with their pins and arguments, and whether it's by key. The same
// shape twice is the same request; a pin fixed after a refusal is another.
func askShape(a connectAsk) string {
	uses := slices.Clone(a.Uses)
	sort.Strings(uses)
	return fmt.Sprintf("%s|%s|%s|%s|%t", appOf(a), a.Reuse, strings.Join(uses, ","), a.Tools, a.API)
}

func appOrReuse(a connectAsk) string {
	if app := appOf(a); app != "" {
		return app
	}
	return "reuse of " + a.Reuse
}

// quotedWords is how many words in a row from an error make a quote of it.
const quotedWords = 6

// wordsOf is s in lower case, as words: letters and digits, an apostrophe
// kept inside one, everything else a break.
func wordsOf(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\'' && r != '’'
	})
}

// quotedValue is what an error quotes back of the request: a tool's title, a
// name, a value Setup itself wrote. Setup may say those again.
var quotedValue = regexp.MustCompile(`"[^"\n]*"|“[^”\n]*”`)

// sixWordsOf is every run of quotedWords words in the errors, each once, never
// across or inside what an error quotes back.
func sixWordsOf(errors []string) map[string]bool {
	out := map[string]bool{}
	for _, e := range errors {
		for _, part := range quotedValue.Split(e, -1) {
			w := wordsOf(part)
			for i := 0; i+quotedWords <= len(w); i++ {
				out[strings.Join(w[i:i+quotedWords], " ")] = true
			}
		}
	}
	return out
}

// quotedError is the first run of quotedWords words of s that an error of the
// run holds, or "".
func quotedError(s string, quoted map[string]bool) string {
	if len(quoted) == 0 {
		return ""
	}
	w := wordsOf(s)
	for i := 0; i+quotedWords <= len(w); i++ {
		if k := strings.Join(w[i:i+quotedWords], " "); quoted[k] {
			return k
		}
	}
	return ""
}

// rawErrors are the backstop for what never comes as an error result: tafs
// cloud's and the runtime's wrapper lines around what Setup reads, error
// codes, and a vendor's status or scope answer as it's usually quoted. Each
// is a regular expression; the first match is reported. The errors the run
// got are read from the run itself (quotedError), never copied here.
var rawErrors = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\[(?:tafs|taf|result|approval|improve taf|did this happen|door knock|connection refused)\b[^\]\n]*\]`),
	regexp.MustCompile(`(?i)\bit answered \d{3}\b`),
	regexp.MustCompile(`(?i)\b(?:HTTP|status(?: code)?)\s*[45]\d\d\b`),
	regexp.MustCompile(`(?i)\b[45]\d\d (?:Forbidden|Unauthorized|Not Found|Bad Request|Internal Server Error)\b`),
	regexp.MustCompile(`(?i)\bEnsure you have the\b`),
	regexp.MustCompile(`(?i)\b(?:insufficient|missing|required) scopes?\b`),
	regexp.MustCompile(`(?i)\bThe owner says:`),
	regexp.MustCompile(`(?i)\bcritical_key_required\b|\bsign_in_not_confirmed\b|\bcards_in_force\b|\bsignature_required\b`),
}

// rawError is the first raw error in s, or "".
func rawError(s string) string {
	for _, re := range rawErrors {
		if m := re.FindString(s); m != "" {
			return m
		}
	}
	return ""
}

func oneLineOf(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		return s[:157] + "…"
	}
	return s
}
