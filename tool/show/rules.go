package show

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// A rules notice says, beside its words, what the owner agrees to for each
// tool the draft will cover: what happens past its limit (Over), and the
// figures of those limits (Limits). The confirm that agrees to them names the
// notice (Component.Agrees), and the owner agrees by tapping its option whose
// value is Agree. tafs cloud checks the draft's Cards against the newest rules
// the owner agreed, at Approve (kiff/tafs, runtime/src/cloud/rules.ts): the
// limits here mirror what it reads, so a notice it would read as carrying no
// rules is refused here instead.
const (
	MaxRules      = 64
	MaxRuleLimits = 4
	MaxRuleTool   = 128
	// MaxRuleLimit is the largest limit, 2^53-1: tafs cloud reads the
	// notice in JavaScript, where a larger integer is rounded, so the figure
	// it checked would not be the one the owner agreed (as KIFF and planque
	// refuse past it).
	MaxRuleLimit = 1<<53 - 1
	// Agree is the value of a confirm's option by which the owner agrees to
	// the rules notice it names.
	Agree = "agree"
)

// Rule is one tool's: Tool is planque's name for it before KIFF's suffix
// (send_reply, not send_reply_k3f9), since the rules are agreed before the
// tools are declared; Over is ask or refuse.
type Rule struct {
	Tool   string  `json:"tool"`
	Over   string  `json:"over"`
	Limits []Limit `json:"limits,omitempty"`
}

// Limit is one figure the owner agreed for a tool: Quantity (count, or
// sum(<arg>)) over Window (call, or calendar_day), at most Limit, in the
// Card's own units (minor units of the connection's currency for an amount).
// Limit is required: 0 is a figure (never), so a pointer tells it from unsaid.
type Limit struct {
	Quantity string `json:"quantity"`
	Window   string `json:"window"`
	Limit    *int64 `json:"limit"`
}

var (
	overs        = []string{"ask", "refuse"}
	ruleWindows  = []string{"call", "calendar_day"}
	sumPattern   = regexp.MustCompile(`^sum\([A-Za-z0-9_.]{1,64}\)$`)
	foldRuleTool = func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
)

func checkRules(rules []Rule) error {
	if rules != nil && len(rules) == 0 {
		return fmt.Errorf("rules, when given, has at least one rule: leave it out for a notice with none")
	}
	if len(rules) > MaxRules {
		return fmt.Errorf("rules has at most %d rules, one per tool", MaxRules)
	}
	tools := map[string]bool{}
	for i, r := range rules {
		name := foldRuleTool(r.Tool)
		switch {
		case name == "":
			return fmt.Errorf("rule %d: tool is required, planque's name before KIFF's suffix", i+1)
		case len(strings.TrimSpace(r.Tool)) > MaxRuleTool:
			return fmt.Errorf("rule %d: tool is at most %d characters", i+1, MaxRuleTool)
		case tools[name]:
			return fmt.Errorf("rule %d: another rule has tool %q; one rule per tool", i+1, r.Tool)
		case !slices.Contains(overs, r.Over):
			return fmt.Errorf("rule %d (%s): over is ask or refuse", i+1, r.Tool)
		case r.Limits != nil && len(r.Limits) == 0:
			return fmt.Errorf("rule %d (%s): limits, when given, has at least one limit: leave it out for none", i+1, r.Tool)
		case len(r.Limits) > MaxRuleLimits:
			return fmt.Errorf("rule %d (%s): at most %d limits", i+1, r.Tool, MaxRuleLimits)
		}
		tools[name] = true
		pairs := map[string]bool{}
		for j, l := range r.Limits {
			at := fmt.Sprintf("rule %d (%s), limit %d", i+1, r.Tool, j+1)
			switch {
			case l.Quantity != "count" && !sumPattern.MatchString(l.Quantity):
				return fmt.Errorf("%s: quantity is count or sum(<arg>), as on the Card", at)
			case !slices.Contains(ruleWindows, l.Window):
				return fmt.Errorf("%s: window is call or calendar_day", at)
			case l.Limit == nil:
				return fmt.Errorf("%s: limit is required, in the Card's units", at)
			case *l.Limit < 0:
				return fmt.Errorf("%s: limit is 0 or more", at)
			case *l.Limit > MaxRuleLimit:
				// No figure here: Coach reads this refusal, and copied the
				// largest limit into a Card once (Eval 16).
				return fmt.Errorf("%s: limit is larger than a Card takes; give the figure the owner agreed, in the Card's own units", at)
			case pairs[l.Quantity+" "+l.Window]:
				return fmt.Errorf("%s: another limit has %s per %s; one figure each", at, l.Quantity, l.Window)
			}
			pairs[l.Quantity+" "+l.Window] = true
		}
	}
	return nil
}

// checkAgrees: a confirm that agrees names a notice by its id, and has the
// option the owner taps to agree.
func checkAgrees(agrees string, options []Option) error {
	if agrees == "" {
		return nil
	}
	if !idPattern.MatchString(agrees) {
		return fmt.Errorf("agrees names the rules notice by its id, 1 to 64 of a-z 0-9 - _")
	}
	if !slices.ContainsFunc(options, func(o Option) bool { return o.Value == Agree }) {
		return fmt.Errorf("a confirm that agrees has an option with value %q, which the owner taps to agree", Agree)
	}
	return nil
}
