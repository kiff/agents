package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/eval"
	"github.com/kiff/agents/session"
)

// The Coach batch's gates (13 Nov 2026): Setup connects an app on the
// first try, offers only connections it made in this conversation, asks for
// a key and its Card in one approval when the connection declared the key's
// tools, and narrows a managed app's sign-in to the tools the taf uses.

// connectAsk is one of Setup's planque requests and what planque answered.
type connectAsk struct {
	Call   string // the tool call's id
	Entry  string
	Server string // an MCP server's URL, when the request names one
	APIURL string // a key request's description URL
	Reuse  string
	Uses   []string
	API    bool
	// Tools is the request's tools as sent, each by its name with its pins
	// and arguments, in a stable form: a request that changes a pin isn't
	// the one before it.
	Tools   string
	Refused string // planque's refusal, its first line; "" when it went through
	Grant   string // the grant planque made, when it went through; for a tools call, the grant it adds to
	// Declared: planque declared the key grant's tools to KIFF at once, so
	// its Card can be drafted before the key and signed with it.
	Declared bool
}

// approvalSeen is an approval card the owner answered: its grant, the draft
// it carried, and whether that grant takes a key. Again: a new card for a
// grant already approved, with nothing left to approve or sign: shown to the
// owner again, never an approval of its own (the no-loop gate counts it).
type approvalSeen struct {
	Grant, Draft string
	Key          bool
	Again        bool
}

// connectAsksOf are the planque requests in a session's events, each paired
// with its result, in the order Setup made them.
func connectAsksOf(events []session.Event) []connectAsk { return planqueAsksOf(events, "request") }

// toolsAsksOf are Setup's calls that add tools to a connected grant (planque's
// tools action), each with its result: a refused one sent again as it was is
// a loop too.
func toolsAsksOf(events []session.Event) []connectAsk { return planqueAsksOf(events, "tools") }

func planqueAsksOf(events []session.Event, action string) []connectAsk {
	calls := map[string]connectAsk{}
	var order []string
	for _, e := range events {
		switch e.Kind {
		case session.ToolCall:
			var c agent.ToolCallBody
			if json.Unmarshal(e.Body, &c) != nil || c.Tool != "planque" {
				continue
			}
			var in struct {
				Action string          `json:"action"`
				Entry  string          `json:"entry"`
				Server string          `json:"server"`
				Reuse  string          `json:"reuse"`
				Grant  string          `json:"grant"`
				Uses   []string        `json:"uses"`
				API    json.RawMessage `json:"api"`
				Tools  json.RawMessage `json:"tools"`
			}
			if json.Unmarshal(c.Input, &in) != nil || in.Action != action {
				continue
			}
			var api struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(in.API, &api)
			a := connectAsk{Call: c.CallID, Entry: in.Entry, Server: in.Server, APIURL: api.URL, Reuse: in.Reuse, Uses: in.Uses,
				API: len(in.API) > 0 && string(in.API) != "null", Tools: toolsShape(in.Tools)}
			if action == "tools" {
				a.Grant = in.Grant
			}
			calls[c.CallID] = a
			order = append(order, c.CallID)
		case session.ToolResult:
			var r agent.ToolResultBody
			if json.Unmarshal(e.Body, &r) != nil {
				continue
			}
			a, ok := calls[r.CallID]
			if !ok {
				continue
			}
			if r.IsError {
				a.Refused, _, _ = strings.Cut(strings.TrimSpace(r.Text), "\n")
			} else if action == "request" {
				var g struct {
					ID       string `json:"id"`
					Declared bool   `json:"declared"`
				}
				_ = json.Unmarshal([]byte(r.Text), &g)
				a.Grant, a.Declared = g.ID, g.Declared
			}
			calls[r.CallID] = a
		}
	}
	out := make([]connectAsk, 0, len(order))
	for _, id := range order {
		out = append(out, calls[id])
	}
	return out
}

// toolsShape is a request's tools in a stable form: each tool's name, pins
// and arguments, sorted by name, keys in order (encoding/json sorts a map's).
// What else a tool carries (its title) doesn't make it another request.
func toolsShape(raw json.RawMessage) string {
	var tools []struct {
		Tool  string `json:"tool"`
		Fixed any    `json:"fixed_arguments,omitempty"`
		Args  any    `json:"arguments,omitempty"`
	}
	if json.Unmarshal(raw, &tools) != nil || len(tools) == 0 {
		return ""
	}
	sort.SliceStable(tools, func(i, j int) bool { return tools[i].Tool < tools[j].Tool })
	b, _ := json.Marshal(tools)
	return string(b)
}

// errorsOf are the texts of every tool result the run got as an error, from
// planque, KIFF, tafs cloud or a vendor, in full: the words that are theirs,
// not the person's to read.
func errorsOf(events []session.Event) []string {
	var out []string
	for _, e := range events {
		if e.Kind != session.ToolResult {
			continue
		}
		var r agent.ToolResultBody
		if json.Unmarshal(e.Body, &r) == nil && r.IsError && strings.TrimSpace(r.Text) != "" {
			out = append(out, r.Text)
		}
	}
	return out
}

// gradeBatch grades the batch's gates over what the run asked of planque and
// the approvals the owner answered. ours are the grants of the tafs this run
// made, from another conversation (an improvement's), which Setup may reuse
// as the product does for a second taf. Each gate is skipped, with why, when
// the run didn't do what it grades.
func gradeBatch(asks []connectAsk, approvals []approvalSeen, ours map[string]bool) []eval.CheckResult {
	var out []eval.CheckResult
	gate := func(name string, g eval.Grade) { out = append(out, eval.CheckResult{Name: name, Grade: g}) }
	skip := func(name, why string) { gate(name, eval.Grade{Skipped: true, Why: why}) }

	// Connects on the first try: no request planque refused. A refused shape
	// was Coach's mistake, shown to the owner as a failed step.
	if len(asks) == 0 {
		skip("connects on the first try", "no connection was asked for")
	} else {
		var refused []string
		for _, a := range asks {
			if a.Refused != "" {
				refused = append(refused, a.Refused)
			}
		}
		if len(refused) > 0 {
			gate("connects on the first try", eval.Grade{Why: fmt.Sprintf("%d request(s) refused: %s", len(refused), strings.Join(refused, " | "))})
		} else {
			gate("connects on the first try", eval.Grade{Pass: true})
		}
	}

	// Offers only its own connections: a reused sign-in, and every approval,
	// names a grant this conversation made or one of this run's tafs holds.
	// The Eval's account has no taf of its own: the sweep archives each
	// run's agents and leaves their grants at planque, so any other grant is
	// an earlier test's, which Setup never offers or reuses.
	made := map[string]bool{}
	for g := range ours {
		made[g] = true
	}
	for _, a := range asks {
		if a.Grant != "" {
			made[a.Grant] = true
		}
	}
	var foreign []string
	for _, a := range asks {
		if a.Reuse != "" && !made[a.Reuse] {
			foreign = append(foreign, "reused "+a.Reuse)
		}
	}
	for _, ap := range approvals {
		if ap.Grant != "" && !made[ap.Grant] {
			foreign = append(foreign, "asked approval for "+ap.Grant)
		}
	}
	switch {
	case len(asks) == 0 && len(approvals) == 0:
		skip("offers only its own connections", "no connection was asked for or approved")
	case len(foreign) > 0:
		gate("offers only its own connections", eval.Grade{Why: "a grant this conversation didn't make: " + strings.Join(foreign, ", ")})
	default:
		gate("offers only its own connections", eval.Grade{Pass: true})
	}

	// One approval for a key and its Card: a key grant planque declared at
	// once has its Card drafted before the key, so an approval carries the
	// draft and one fingerprint signs both. With two key apps and one Card,
	// one app's key comes alone and the other's carries the Card: two
	// approvals, one per app. What fails is a Card asked for apart from the
	// keys (a key approved, then its Card), or no key approval carrying it.
	// A key grant that wasn't declared takes the key first, as documented:
	// nothing to grade.
	declared := map[string]bool{}
	for _, a := range asks {
		if a.Declared && a.Grant != "" {
			declared[a.Grant] = true
		}
	}
	first := map[string]approvalSeen{}
	var keys, apart []string
	connectedByKey, withCard := false, false
	for _, ap := range approvals {
		if !ap.Key {
			continue
		}
		connectedByKey = true
		if !declared[ap.Grant] {
			continue
		}
		f, seen := first[ap.Grant]
		switch {
		case !seen:
			first[ap.Grant] = ap
			keys = append(keys, ap.Grant)
			withCard = withCard || ap.Draft != ""
		case ap.Draft != "" && f.Draft == "":
			// The key approved first, its Card in an approval of its own.
			apart = append(apart, ap.Grant)
		}
	}
	switch {
	case !connectedByKey:
		skip("one approval for the key and the Card", "no app was connected by key")
	case len(keys) == 0:
		skip("one approval for the key and the Card", "no key grant was declared, so its key comes before its Card")
	default:
		var alone []string
		for _, g := range keys {
			if first[g].Draft == "" {
				alone = append(alone, g)
			}
		}
		switch {
		case len(apart) > 0:
			gate("one approval for the key and the Card", eval.Grade{Why: "the key was asked for before its Card: " + strings.Join(apart, ", ")})
		case !withCard:
			gate("one approval for the key and the Card", eval.Grade{Why: "the key was asked for before its Card: " + strings.Join(alone, ", ")})
		default:
			gate("one approval for the key and the Card", eval.Grade{Pass: true})
		}
	}

	// Narrows a managed app's sign-in: a request for a provider's app names
	// the tools the taf uses (uses), so the sign-in asks only for those.
	var managed, wide []string
	for _, a := range asks {
		if strings.HasPrefix(a.Entry, "connector:") {
			managed = append(managed, a.Entry)
			if len(a.Uses) == 0 {
				wide = append(wide, a.Entry)
			}
		}
	}
	switch {
	case len(managed) == 0:
		skip("narrows a managed sign-in", "no managed app was connected")
	case len(wide) > 0:
		gate("narrows a managed sign-in", eval.Grade{Why: "asked with no uses, so the sign-in asks for the app's whole access: " + strings.Join(slices.Compact(wide), ", ")})
	default:
		gate("narrows a managed sign-in", eval.Grade{Pass: true})
	}
	return out
}
