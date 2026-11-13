package main

import (
	"encoding/json"
	"fmt"
	"slices"
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
	Call    string // the tool call's id
	Entry   string
	Server  string // an MCP server's URL, when the request names one
	APIURL  string // a key request's description URL
	Reuse   string
	Uses    []string
	API     bool
	Refused string // planque's refusal, its first line; "" when it went through
	Grant   string // the grant planque made, when it went through
	// Declared: planque declared the key grant's tools to KIFF at once, so
	// its Card can be drafted before the key and signed with it.
	Declared bool
}

// approvalSeen is an approval card the owner answered: its grant, the draft
// it carried, and whether that grant takes a key.
type approvalSeen struct {
	Grant, Draft string
	Key          bool
}

// connectAsksOf are the planque requests in a session's events, each paired
// with its result, in the order Setup made them.
func connectAsksOf(events []session.Event) []connectAsk {
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
				Uses   []string        `json:"uses"`
				API    json.RawMessage `json:"api"`
			}
			if json.Unmarshal(c.Input, &in) != nil || in.Action != "request" {
				continue
			}
			var api struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(in.API, &api)
			calls[c.CallID] = connectAsk{Call: c.CallID, Entry: in.Entry, Server: in.Server, APIURL: api.URL, Reuse: in.Reuse, Uses: in.Uses, API: len(in.API) > 0 && string(in.API) != "null"}
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
			} else {
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
	// once has its Card drafted before the key, so its first approval
	// carries the draft and one fingerprint signs both. A key grant that
	// wasn't declared takes the key first, as documented: nothing to grade.
	declared := map[string]bool{}
	for _, a := range asks {
		if a.Declared && a.Grant != "" {
			declared[a.Grant] = true
		}
	}
	first := map[string]approvalSeen{}
	var keys []string
	connectedByKey := false
	for _, ap := range approvals {
		if !ap.Key {
			continue
		}
		connectedByKey = true
		if _, seen := first[ap.Grant]; !seen && declared[ap.Grant] {
			first[ap.Grant] = ap
			keys = append(keys, ap.Grant)
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
		if len(alone) > 0 {
			gate("one approval for the key and the Card", eval.Grade{Why: "the key was asked for before its Card: " + strings.Join(alone, ", ")})
		} else {
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
