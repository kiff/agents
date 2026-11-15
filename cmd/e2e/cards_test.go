package main

import (
	"encoding/json"
	"testing"

	"github.com/kiff/agents/tool/show"
)

// The scripted owner's answers reach a waiting card as the app sends them:
// as data in the card's shape, and their own words as written instead.
func TestTheOwnersAnswerToAWaitingCard(t *testing.T) {
	parse := func(raw string) show.Component {
		c, err := show.Parse(json.RawMessage(raw))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	confirm := parse(`{"type":"confirm","id":"mode","text":"Test or live?","options":[{"value":"test","label":"Test mode"},{"value":"live","label":"Live"}]}`)
	approval := parse(`{"type":"approval","id":"connect","grant":"grt_1"}`)
	signIn := parse(`{"type":"sign_in","id":"mail","grant":"grt_2","service":"mail.example.com"}`)
	for _, c := range []struct {
		card show.Component
		msg  string
		want string
	}{
		{confirm, "[confirm mode] test", `{"answer":"test","words":"Test mode"}`},
		{approval, "[approval connect] approved, signed in", `{"answer":"approved","words":"Approved"}`},
		// A draft tafs cloud's check refused is answered with its words, never "approved" (Eval 18's Refunds).
		{approval, "[approval connect] failed: the draft counts it on two Cards. Draft it again now and show a new approval.",
			`{"answer":"failed: the draft counts it on two Cards. Draft it again now and show a new approval.","words":"Approve · it didn't go through: the draft counts it on two Cards. Draft it again now and show a new approval."}`},
		{approval, "[approval connect] already approved: Stripe is connected", `{"answer":"already approved: Stripe is connected","words":"Already approved"}`},
		{signIn, "[sign_in mail] failed: the sign-in closed", `{"answer":"failed: the sign-in closed","words":"Sign in · it didn't go through: the sign-in closed"}`},
		{confirm, "300 EUR a day, 50 each", `{"answer":null,"words":"In your words: “300 EUR a day, 50 each”","wrote":"300 EUR a day, 50 each"}`},
	} {
		got, err := cardAnswer(c.card, c.msg)
		if err != nil || string(got) != c.want {
			t.Errorf("%q: %s, %v; want %s", c.msg, got, err, c.want)
		}
	}
	if _, err := cardAnswer(confirm, "[confirm mode] staging"); err == nil {
		t.Error("an answer the card doesn't take was sent")
	}
}
