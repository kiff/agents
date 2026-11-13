package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/session"
)

// A taf's dial (its schedule, where its results go) asked about during
// setup, apart from the Card: the owner's switch to a dial version (a knob
// card, or version_install held for them) is an approval of its own, and a
// version drafted while setting the taf up is a loop. The bar wants the dial
// to ride in the Card's approval, and the taf set up as its first version.

// The kinds of dialSeen.
const (
	dialCard    = "card"    // a knob card: the dial version, with the switch to it
	dialInstall = "install" // version_install, held for the owner
	dialDraft   = "draft"   // a version drafted (taf_dial, or taf_results with a place at an app)
)

// dialSeen is one of those, read from the setup run.
type dialSeen struct {
	Key     string // the card's id, or the tool call's
	Kind    string
	Taf     string
	Version int
}

// isTafsTool says name is the tafs tool want, as KIFF's gateway names it.
func isTafsTool(name, want string) bool {
	return name == want || strings.HasSuffix(name, "_"+want) || strings.HasSuffix(name, "."+want)
}

// dialsOf reads a run's dial cards, installs and drafted versions. An
// install refused back to the model unmade never reached the owner; a draft
// counts once tafs cloud made the version.
func dialsOf(events []session.Event) []dialSeen {
	var out []dialSeen
	drafting := map[string]bool{}
	installs := map[string]int{}
	for _, e := range events {
		switch e.Kind {
		case session.Component:
			var c struct {
				Type    string `json:"type"`
				ID      string `json:"id"`
				Taf     string `json:"taf_id"`
				Version int    `json:"version"`
			}
			if json.Unmarshal(e.Body, &c) == nil && c.Type == "knob" {
				out = append(out, dialSeen{Key: c.ID, Kind: dialCard, Taf: c.Taf, Version: c.Version})
			}
		case session.ToolCall:
			var c agent.ToolCallBody
			if json.Unmarshal(e.Body, &c) != nil {
				continue
			}
			switch {
			case isTafsTool(c.Tool, "version_install"):
				var in struct {
					Taf string `json:"taf"`
					N   int    `json:"n"`
				}
				_ = json.Unmarshal(c.Input, &in)
				installs[c.CallID] = len(out)
				out = append(out, dialSeen{Key: c.CallID, Kind: dialInstall, Taf: in.Taf, Version: in.N})
			case isTafsTool(c.Tool, "taf_dial"), isTafsTool(c.Tool, "taf_results"):
				drafting[c.CallID] = true
			}
		case session.ToolResult:
			var r agent.ToolResultBody
			if json.Unmarshal(e.Body, &r) != nil {
				continue
			}
			if i, ok := installs[r.CallID]; ok && r.NotMade {
				out[i].Kind = ""
			}
			if !drafting[r.CallID] || r.IsError {
				continue
			}
			var made struct {
				Taf     string `json:"taf_id"`
				Version int    `json:"version"`
			}
			if json.Unmarshal([]byte(r.Text), &made) == nil && made.Version > 0 {
				out = append(out, dialSeen{Key: r.CallID, Kind: dialDraft, Taf: made.Taf, Version: made.Version})
			}
		}
	}
	kept := out[:0]
	for _, d := range out {
		if d.Kind != "" {
			kept = append(kept, d)
		}
	}
	return kept
}

// keptDials adds a run's dials to those already read, each once.
func keptDials(kept, dials []dialSeen) []dialSeen {
	for _, d := range dials {
		seen := false
		for _, k := range kept {
			if k.Key == d.Key && k.Kind == d.Kind {
				seen = true
				break
			}
		}
		if !seen {
			kept = append(kept, d)
		}
	}
	return kept
}

// dialApprovals are the owner's approvals of a dial version, apart from the
// Card: a knob card and the install held for the same version are one.
func dialApprovals(dials []dialSeen) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range dials {
		if d.Kind != dialCard && d.Kind != dialInstall {
			continue
		}
		k := fmt.Sprintf("%s#%d", d.Taf, d.Version)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, fmt.Sprintf("the switch to dial version %d asked apart from the Card", d.Version))
	}
	return out
}

// dialLoops are the versions drafted during setup, each once.
func dialLoops(dials []dialSeen) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range dials {
		k := fmt.Sprintf("%s#%d", d.Taf, d.Version)
		if d.Kind != dialDraft || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, fmt.Sprintf("version %d of the taf drafted during setup", d.Version))
	}
	return out
}
