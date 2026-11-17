package main

import (
	"fmt"
	"strings"
)

// unagreedFigures are the limits KIFF holds on the taf's Cards that the agreed
// rules, as data, don't name: on a tool whose rule has figures, each call's cap
// (per_action_max, window "call") and each total with no per, in any window,
// whose quantity and window no figure of the rule gives. A fact the harness
// reads, never a vote: in Eval 43927756622 (Setup, run 1), after a refused
// draft, Coach signed an hour's €50 held on the refunds beside the agreed €5 a
// refund and €50 a day refused, and the judge alone caught it, in 1 run of 3.
// A total for each item (per) isn't named here (the judge reads it), nor a
// count of 0 (each call asks). Each is "tool quantity per window limit", the
// tool by its KIFF name.
func unagreedFigures(s said, cards []map[string]any) []string {
	notice, _, ok := agreedNotice(s)
	if !ok {
		return nil
	}
	var out []string
	for _, tl := range s.connected {
		var rule *shownRule
		for i, r := range notice.Rules {
			if foldTool(r.Tool) == foldTool(tl.Tool) || foldTool(r.Tool) == foldTool(tl.Name) {
				rule = &notice.Rules[i]
				break
			}
		}
		if rule == nil || len(rule.Limits) == 0 {
			continue
		}
		name := tl.Name
		if name == "" {
			name = tl.Tool
		}
		namesPer := false
		for _, l := range rule.Limits {
			namesPer = namesPer || strings.TrimSpace(l.Per) != ""
		}
		for _, f := range figuresHeld(cards, foldTool(name)) {
			if f.quantity == "count" && f.limit == int64(0) {
				continue
			}
			if f.per != "" && !namesPer && !perTotalsAgreed {
				continue
			}
			named := false
			for _, l := range rule.Limits {
				if strings.TrimSpace(l.Quantity) == f.quantity && foldTool(l.Window) == f.window && foldTool(l.Per) == f.per {
					named = true
					break
				}
			}
			if !named {
				each := ""
				if f.per != "" {
					each = " for each " + f.per
				}
				out = append(out, fmt.Sprintf("%s %s per %s%s %v", name, f.quantity, f.window, each, f.limit))
			}
		}
	}
	return out
}

// perTotalsAgreed: every total for each item must be named by a figure, on a
// rule that names none too. Off until the definitions that put per totals in
// the rules are upgraded; tafs cloud's TAFS_RULES_PER switches with it.
const perTotalsAgreed = false

type heldFigure struct {
	quantity, window, per string
	limit                 any
}

// figuresHeld are the figures the Cards set on one tool (its KIFF name,
// folded): each call's cap from its grants' per_action_max, and each total
// from the Cards' aggregates, in the window it names, with its per (folded)
// when it is a total for each item.
func figuresHeld(cards []map[string]any, name string) []heldFigure {
	var out []heldFigure
	for _, c := range cards {
		covers := false
		for _, g := range asList(c["grants"]) {
			action, _ := g["action"].(string)
			if foldTool(action) != name {
				continue
			}
			covers = true
			for _, m := range asList(g["per_action_max"]) {
				q, _ := m["quantity"].(string)
				out = append(out, heldFigure{strings.TrimSpace(q), "call", "", number(m["max"])})
			}
		}
		if !covers {
			continue
		}
		for i, a := range asList(c["aggregates"]) {
			if kiffsOwnCount(cards, c, i) {
				continue
			}
			q, _ := a["quantity"].(string)
			w, _ := a["window"].(string)
			per, _ := a["per"].(string)
			out = append(out, heldFigure{strings.TrimSpace(q), foldTool(w), foldTool(per), number(a["limit"])})
		}
	}
	return out
}

// kiffCountADay is KIFF's own count, never the owner's: KIFF refuses a Card
// with no total, so a Card the owner gave none carries 10,000 calls a day,
// "KIFF adds it". The reads' Card has it, and so does a held Card whose only
// figure is each call's cap, beside a refused Card with the day's total on the
// same tool ("ask past €5 a refund, refuse past €50 a day"; Eval 43975057022,
// Setup run 3). The same as tafs cloud's kiffsOwnCount.
const kiffCountADay = 10000

// kiffsOwnCount reports whether card's aggregate i is KIFF's own count: a
// count of kiffCountADay a calendar day, for no item, as the only total of a
// Card that holds (not one that refuses), with every tool the Card covers also
// on another Card with a total on an amount over all items (never one item's).
// Alone, or beside €50 for each customer, it would let 10,000 calls under the
// cap through a day unasked, so it's a figure like any other.
func kiffsOwnCount(cards []map[string]any, card map[string]any, i int) bool {
	aggs := asList(card["aggregates"])
	if onExceed, _ := card["on_exceed"].(string); onExceed == "refuse" || len(aggs) != 1 || i != 0 {
		return false
	}
	a := aggs[0]
	q, _ := a["quantity"].(string)
	w, _ := a["window"].(string)
	per, _ := a["per"].(string)
	if foldTool(q) != "count" || foldTool(w) != "calendar_day" || foldTool(per) != "" || number(a["limit"]) != any(int64(kiffCountADay)) {
		return false
	}
	var actions []string
	for _, g := range asList(card["grants"]) {
		if action, _ := g["action"].(string); foldTool(action) != "" {
			actions = append(actions, foldTool(action))
		}
	}
	if len(actions) == 0 {
		return false
	}
	for _, action := range actions {
		bounded := false
		for _, o := range cards {
			if sameCard(o, card) {
				continue
			}
			covers := false
			for _, g := range asList(o["grants"]) {
				if a, _ := g["action"].(string); foldTool(a) == action {
					covers = true
				}
			}
			for _, t := range asList(o["aggregates"]) {
				q, _ := t["quantity"].(string)
				per, _ := t["per"].(string)
				if covers && strings.HasPrefix(foldTool(q), "sum(") && foldTool(per) == "" {
					bounded = true
				}
			}
		}
		if !bounded {
			return false
		}
	}
	return true
}

// sameCard is whether a and b are the same Card of the draft (the same map).
func sameCard(a, b map[string]any) bool {
	return fmt.Sprintf("%p", a) == fmt.Sprintf("%p", b)
}

// number is a JSON number as an int64 when it's whole, else as read.
func number(v any) any {
	if f, ok := v.(float64); ok && f == float64(int64(f)) {
		return int64(f)
	}
	return v
}
