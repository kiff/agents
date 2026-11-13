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
	Reuse   string
	Uses    []string
	API     bool
	Refused string // planque's refusal, its first line; "" when it went through
	Grant   string // the grant planque made, when it went through
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
				Reuse  string          `json:"reuse"`
				Uses   []string        `json:"uses"`
				API    json.RawMessage `json:"api"`
			}
			if json.Unmarshal(c.Input, &in) != nil || in.Action != "request" {
				continue
			}
			calls[c.CallID] = connectAsk{Call: c.CallID, Entry: in.Entry, Reuse: in.Reuse, Uses: in.Uses, API: len(in.API) > 0 && string(in.API) != "null"}
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
					ID string `json:"id"`
				}
				_ = json.Unmarshal([]byte(r.Text), &g)
				a.Grant = g.ID
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

	// One approval for a key and its Card: a key grant's first approval
	// carries the Card's draft, so one fingerprint signs both. A case that
	// ends once the app is connected approves no Card: nothing to grade.
	carded := slices.ContainsFunc(approvals, func(ap approvalSeen) bool { return ap.Draft != "" })
	first := map[string]approvalSeen{}
	var keys []string
	for _, ap := range approvals {
		if !ap.Key {
			continue
		}
		if _, seen := first[ap.Grant]; !seen {
			first[ap.Grant] = ap
			keys = append(keys, ap.Grant)
		}
	}
	switch {
	case len(keys) == 0:
		skip("one approval for the key and the Card", "no app was connected by key")
	case !carded:
		skip("one approval for the key and the Card", "no Card was approved in this case")
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
