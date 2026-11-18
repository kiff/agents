package main

// The weekly proof's approval (kiff-cloud#1716, PM 05:42): a real Coach chat
// on tafs cloud, its approval card answered only through tafs cloud's
// component route, as the owner's phone answers it. This file is the phone's
// side of that route: what it checks before it signs, ported from the
// Android app (kiff/tafs AppModel.expectedApprovalMessage and stepMatchesMark).
//
// The phone is the last check, not a signing service: it signs only the
// message the card it showed implies, rebuilt from what it holds (its account,
// the card's grant at the digest shown, the card's draft at the version
// shown), and refuses any other. The weekly proof asks only for the two-step
// flow (no `can`), so the steps it signs are connect and cards.

import (
	"bytes"
	"encoding/json"
	"slices"
)

// weeklyCard is what an approval card showed the owner: its grant at the
// digest shown, its draft at the version shown, KIFF's words for the
// connection's profile if shown, and tafs cloud's mark (whether the card drew
// its Card as signed by this tap), nil for no mark.
type weeklyCard struct {
	Grant, Draft            string
	ShownDigest, ShownDraft string
	ShownProfile            json.RawMessage
	SignsCard               *bool
	// Tools are the connection's tools as the card showed them, by the
	// catalog's names.
	Tools []string
}

// weeklyAsk is tafs cloud's ask to sign (outcome "sign").
type weeklyAsk struct {
	ID      string `json:"id"`
	Step    string `json:"step"`
	Message string `json:"message"`
	Scheme  string `json:"scheme"`
	Reason  string `json:"reason"`
	Key     string `json:"key"`
	Planque *struct {
		Grant string          `json:"grant"`
		Body  json.RawMessage `json:"body"`
	} `json:"planque"`
	KIFF *struct {
		Draft   string `json:"draft"`
		Version string `json:"version"`
	} `json:"kiff"`
}

// expectedApprovalMessage is the message this card's approval may sign, or ""
// for an ask the phone can't rebuild from what the card showed: another
// grant, draft, digest or version, another approve body, or a step the
// weekly proof never asks for.
func expectedApprovalMessage(ask weeklyAsk, c weeklyCard, tenant string) string {
	switch ask.Step {
	case "connect":
		if ask.Planque == nil || ask.Planque.Grant != c.Grant || c.ShownDigest == "" {
			return ""
		}
		var body struct {
			Digest string `json:"digest"`
		}
		if json.Unmarshal(ask.Planque.Body, &body) != nil || body.Digest != c.ShownDigest {
			return ""
		}
		// The whole approve body is the one the phone builds from what was
		// shown: anything added, or other profile words, is never signed.
		if !sameJSON(ask.Planque.Body, shownConnectBody(c.ShownDigest, c.ShownProfile)) {
			return ""
		}
		return "tafs.connect.v1|" + tenant + "|" + c.Grant + "|" + body.Digest
	case "cards":
		if ask.KIFF == nil || c.Draft == "" || ask.KIFF.Draft != c.Draft || c.ShownDraft == "" || ask.KIFF.Version != c.ShownDraft {
			return ""
		}
		return "tafs.cards.v1|" + tenant + "|" + c.Draft + "|" + ask.KIFF.Version
	}
	return ""
}

// shownConnectBody is planque's approve body for what the card showed: the
// digest, and kiff_profile only when KIFF's words were shown.
func shownConnectBody(digest string, profile json.RawMessage) json.RawMessage {
	m := map[string]json.RawMessage{}
	d, _ := json.Marshal(digest)
	m["digest"] = d
	if len(profile) > 0 {
		m["kiff_profile"] = profile
	}
	b, _ := json.Marshal(m)
	return b
}

// sameJSON reports whether two JSON documents are equal as values.
func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ja, _ := json.Marshal(x)
	jb, _ := json.Marshal(y)
	return bytes.Equal(ja, jb)
}

// stepMatchesMark is whether an ask's step is what the card drew: a card that
// showed its Card as signed takes only a step that signs it; one that showed
// it as next, any other step; no mark, any step.
func stepMatchesMark(signsCard *bool, step string) bool {
	if signsCard == nil {
		return true
	}
	return *signsCard == slices.Contains([]string{"cards", "key_and_cards", "tools_and_cards"}, step)
}

// sameTools is kiff-qa's condition (c): the phone signs a connection only
// when the tools it asks for are exactly the card's and the family's
// fixture's. A broader ask is refused on the phone and fails the run as
// "asked for more than the job".
func sameTools(asked, card, fixture []string) bool {
	norm := func(s []string) []string {
		out := slices.Clone(s)
		slices.Sort(out)
		return slices.Compact(out)
	}
	a, c, f := norm(asked), norm(card), norm(fixture)
	return len(a) > 0 && slices.Equal(a, c) && slices.Equal(a, f)
}
