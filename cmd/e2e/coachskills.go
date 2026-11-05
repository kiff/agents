package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// Which of Coach's skills a conversation opens with, as tafs cloud decides it
// (kiff/tafs#525, runtime/src/cloud/coach-skills.ts, way 1): the e2e drives
// Setup without tafs cloud, so it names them the same way, from the entry
// point. Only skills the definition names are sent; an older definition
// (kiff/tafs#218's names, or none) sends none, and the case runs as before.
var coachStart = map[string][]string{
	"free":    {"set-up-a-taf", "connect-an-app", "name-the-tools", "draft-the-card", "usage-and-plans"},
	"improve": {"improve-a-taf", "connect-an-app", "name-the-tools", "draft-the-card"},
}

// startSkills is the entry point's skills as stored versions, in the
// definition's order, as kiff/agents keeps them.
func (t *test) startSkills(entry string) []string {
	want := coachStart[entry]
	var out []string
	for _, ref := range t.setupSkillRefs {
		name, _, _ := strings.Cut(ref, "@")
		if slices.Contains(want, name) {
			out = append(out, ref)
		}
	}
	return out
}

// loadedOrRead reports whether a session has skill (by name): loaded by
// tafs cloud's rule on the session, or read by the agent (read_skill), and
// which. An error reads nothing.
func (t *test) loadedOrRead(ctx context.Context, sessionID, name string) (how string, err error) {
	s, _, err := t.sessions.Read(ctx, t.tenant, sessionID)
	if err != nil {
		return "", err
	}
	for _, ref := range s.Skills {
		if n, _, _ := strings.Cut(ref, "@"); n == name {
			return "loaded", nil
		}
	}
	read, err := t.skillsRead(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if slices.Contains(read, name) {
		return "read", nil
	}
	return "", nil
}

// hasIt fails when a session has neither loaded nor read the skill it's about.
func (t *test) hasIt(ctx context.Context, sessionID, want, what string) error {
	how, err := t.loadedOrRead(ctx, sessionID, want)
	if err != nil {
		return err
	}
	fmt.Fprintf(t.cfg.log, "skill %s %s: %s\n", want, what, map[string]string{"": "neither loaded nor read", "loaded": "loaded by the start", "read": "read by the agent"}[how])
	if how == "" {
		return fmt.Errorf("%s had no %s: neither loaded by the start nor read", what, want)
	}
	return nil
}

// What Setup may say of a connection's mode depends on what the connection
// reports (kiff/tafs#525's core rule). planque's fake vendor reports test (its
// accounts read, a charge's livemode false), so saying test is what it reports,
// and saying live or real money is a claim. The desk reports no mode at all,
// so any mode said as fact is a claim.

// statedModes and statedLive are the words that state a mode, and a live one.
var statedModes = []string{"test mode", "modo de prueba", "no real money", "sin dinero real", "live mode", "modo real", "real money", "dinero real"}
var statedLive = []string{"live mode", "modo real", "real money", "dinero real"}

// claimsMode is the first of Setup's texts that states a mode the connection
// doesn't report, "" when none: with reportsTest, a live mode or real money
// (not "no real money"); with none reported, any mode.
func claimsMode(texts []string, reportsTest bool) string {
	words := statedModes
	if reportsTest {
		words = statedLive
	}
	for _, t := range texts {
		l := strings.ToLower(t)
		for _, w := range words {
			for i := strings.Index(l, w); i >= 0; {
				before := l[:i]
				negated := strings.HasSuffix(before, "no ") || strings.HasSuffix(before, "sin ")
				if !negated || !reportsTest {
					return t
				}
				next := strings.Index(l[i+len(w):], w)
				if next < 0 {
					break
				}
				i += len(w) + next
			}
		}
	}
	return ""
}
