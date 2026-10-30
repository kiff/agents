package show_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kiff/agents/tool/show"
)

func card(t *testing.T, raw string) show.Component {
	t.Helper()
	c, err := show.Parse(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Each answerable card takes its own answer, checked against the card, and
// gives back the owner's choice as the card showed it.
func TestACardsAnswerIsCheckedAgainstTheCard(t *testing.T) {
	confirm := card(t, `{"type":"confirm","id":"which","text":"Which account?","options":[{"value":"live","label":"Live"},{"value":"test","label":"Test mode"}]}`)
	questions := card(t, `{"type":"questions","id":"limits","text":"Limits","questions":[`+
		`{"id":"mode","text":"Mode?","options":[{"value":"test","label":"Test mode"},{"value":"live","label":"Live"}]},`+
		`{"id":"days","text":"Days?","multi":true,"options":[{"value":"mon","label":"Monday"},{"value":"tue","label":"Tuesday"}]},`+
		`{"id":"cap","text":"How much?","options":[{"value":"100","label":"100 €"},{"value":"200","label":"200 €"}]}]}`)
	approval := card(t, `{"type":"approval","id":"connect-stripe","grant":"grt_1"}`)
	signIn := card(t, `{"type":"sign_in","id":"stripe","grant":"grt_1","service":"mcp.stripe.com","no":"Later"}`)
	for _, c := range []struct {
		card   show.Component
		answer string
		words  string
	}{
		{confirm, `"test"`, "Test mode"},
		{questions, `{"mode":{"value":"test"},"days":{"values":["mon","tue"]},"cap":{"own":"300 € on weekdays"}}`, "Test mode · Monday, Tuesday · In your words: “300 € on weekdays”"},
		{approval, `"approved"`, "Approved"},
		{approval, `"declined"`, "Declined"},
		{approval, `"failed: the phone couldn't sign"`, "Approve · it didn't go through: the phone couldn't sign"},
		{signIn, `"signed in"`, "Signed in"},
		{signIn, `"not now"`, "Later"},
	} {
		got, err := show.Answer(c.card, json.RawMessage(c.answer))
		if err != nil || got.Words != c.words || string(got.Answer) != c.answer {
			t.Errorf("%s %s: %+v, %v; want words %q", c.card.Type, c.answer, got, err, c.words)
		}
	}
	for _, c := range []struct {
		card   show.Component
		answer string
		why    string
	}{
		{confirm, `"staging"`, "isn't one of the card's options"},
		{confirm, `["test"]`, "is a string"},
		{questions, `{"mode":{"value":"test"},"days":{"values":["mon"]}}`, `question "cap" isn't answered`},
		{questions, `{"mode":{"value":"test"},"days":{"value":"mon"},"cap":{"own":"x"},"extra":{"own":"y"}}`, `no question "extra"`},
		{questions, `{"mode":{"values":["test"]},"days":{"values":["mon"]},"cap":{"own":"x"}}`, "only when it's multi"},
		{questions, `{"mode":{"value":"test","own":"x"},"days":{"values":["mon"]},"cap":{"own":"x"}}`, "one of value, values or own"},
		{questions, `{"mode":{"value":"prod"},"days":{"values":["mon"]},"cap":{"own":"x"}}`, `isn't one of question "mode"'s options`},
		{approval, `"yes"`, `"approved", "declined" or "failed: <why>"`},
		{approval, `"failed:"`, "followed by why"},
		{signIn, `"approved"`, `"signed in", "not now" or "failed: <why>"`},
		{card(t, `{"type":"notice","id":"n","tone":"info","text":"Hi"}`), `"ok"`, "isn't answered"},
	} {
		if _, err := show.Answer(c.card, json.RawMessage(c.answer)); err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%s %s: %v; want it refused with %q", c.card.Type, c.answer, err, c.why)
		}
	}
}

// An answerable card makes the run wait for its answer; the others don't.
func TestOnlyAnAnswerableCardWaits(t *testing.T) {
	for raw, waits := range map[string]bool{
		`{"type":"confirm","id":"which","text":"Which?","options":[{"value":"a","label":"A"},{"value":"b","label":"B"}]}`: true,
		`{"type":"approval","id":"connect","grant":"grt_1"}`:                                                              true,
		`{"type":"notice","id":"n","tone":"info","text":"Done."}`:                                                         false,
		`{"type":"steps","id":"plan","steps":[{"label":"Look","status":"active"}]}`:                                       false,
	} {
		res, err := show.Tool{}.Call(t.Context(), json.RawMessage(raw))
		if err != nil || res.IsError || res.Waits() != waits {
			t.Errorf("%s: %+v, %v; want waits %v", raw, res, err, waits)
		}
	}
}

// A result waits for what came of the hand-over, and its answer is typed:
// the new taf's id for a new taf, the new version for an improvement, or why
// neither was made. Its words say which, never the model's guess.
func TestAResultIsAnsweredWithWhatCameOfIt(t *testing.T) {
	newTaf := card(t, `{"type":"result","id":"done","text":"Refunds is ready.","taf":{"agent":"refunds","name":"Refunds","job":"refunds","draft":"drf_1"}}`)
	improves := card(t, `{"type":"result","id":"v","text":"A new version.","taf":{"agent":"refunds","name":"Refunds","job":"refunds","improves":"taf_1","note":"asks first"}}`)
	res, err := show.Tool{}.Call(t.Context(), json.RawMessage(`{"type":"result","id":"done","text":"Refunds is ready.","taf":{"agent":"refunds","name":"Refunds","job":"refunds","draft":"drf_1"}}`))
	if err != nil || !res.Waits() || !strings.Contains(res.Text, "what came of the hand-over") {
		t.Fatalf("a result: %+v, %v; want it to wait for the hand-over", res, err)
	}
	for _, c := range []struct {
		card   show.Component
		answer string
		words  string
	}{
		{newTaf, `{"taf_id":"taf_9"}`, "Refunds is set up"},
		{improves, `{"version":3}`, "Version 3 of Refunds is ready to try"},
		{newTaf, `{"failed":"the name is taken"}`, "It didn't go through: the name is taken"},
	} {
		got, err := show.Answer(c.card, json.RawMessage(c.answer))
		if err != nil || got.Words != c.words || string(got.Answer) != c.answer {
			t.Errorf("%s: %+v, %v; want words %q", c.answer, got, err, c.words)
		}
	}
	for _, c := range []struct {
		card   show.Component
		answer string
		why    string
	}{
		{newTaf, `"done"`, `{"taf_id": "…"}`},
		{newTaf, `{}`, "exactly one"},
		{newTaf, `{"taf_id":"taf_9","failed":"x"}`, "exactly one"},
		{newTaf, `{"taf_id":"taf_9","by":"me"}`, `{"taf_id": "…"}`},
		{newTaf, `{"version":2}`, "sets up a new taf"},
		{improves, `{"taf_id":"taf_9"}`, "improves a taf"},
		{improves, `{"version":-1}`, "from 1"},
		{newTaf, `{"failed":"  "}`, "failed says why"},
	} {
		if _, err := show.Answer(c.card, json.RawMessage(c.answer)); err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%s: %v; want it refused with %q", c.answer, err, c.why)
		}
	}
}
