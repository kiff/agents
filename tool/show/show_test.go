package show_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/show"
)

func call(t *testing.T, input string) tool.Result {
	t.Helper()
	r, err := show.New().Call(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Each of contract 1's components, as the agent sends it, is shown as is.
func TestEveryComponentIsShown(t *testing.T) {
	for _, in := range []string{
		`{"type":"steps","id":"plan","title":"Connect Stripe","steps":[{"label":"Ask for access","status":"done"},{"label":"Sign in","status":"active"}]}`,
		`{"type":"notice","id":"n1","tone":"warning","text":"The link expired."}`,
		`{"type":"confirm","id":"plan-ok","text":"Go ahead?","options":[{"value":"yes","label":"Go ahead"},{"value":"no","label":"Not now"}]}`,
		// Several questions answered together (kiff/tafs#138 item 57), every word the agent's.
		`{"type":"questions","id":"setup-basics","text":"Three things before I connect Stripe:","questions":[` +
			`{"id":"mode","text":"Test mode or real money?","options":[{"value":"test","label":"Test mode","description":"Recommended to start"},{"value":"live","label":"Real money"}]},` +
			`{"id":"limit","text":"How much a day before it asks you?","options":[{"value":"10000","label":"100 €"},{"value":"25000","label":"250 €"}],"note":"Each refund over 50 € already waits for you."},` +
			`{"id":"tools","text":"What may it do?","multi":true,"options":[{"value":"refund","label":"Refund"},{"value":"read","label":"Look up payments"}]}]}`,
		`{"type":"approval","id":"connect-stripe","grant":"grt_AbC-1","draft":"draft_9"}`,
		`{"type":"sign_in","id":"connect-stripe","grant":"grt_AbC-1","service":"mcp.stripe.com"}`,
		// The card's own words, in the owner's language (kiff/tafs#75).
		`{"type":"sign_in","id":"entrar","grant":"grt_AbC-1","service":"mcp.stripe.com","title":"Inicia sesión en Stripe","text":"Se abre la página de Stripe.","yes":"Abrir","no":"Ahora no"}`,
		`{"type":"result","id":"listo","taf":{"agent":"refunds","name":"Reembolsos","job":"refunds","draft":"drf_01"},"text":"Listo.","yes":"Ver Reembolsos"}`,
		`{"type":"result","id":"done","taf":{"agent":"refunds","name":"Refunds","job":"refunds","draft":"drf_01"},"text":"Refunds is ready.","tools":["stripe_refund"]}`,
		// Its tools can't find its work: no schedule, the owner hands it work (kiff/tafs#138).
		`{"type":"result","id":"chat","taf":{"agent":"refunds","name":"Refunds","job":"refunds","draft":"drf_01","unscheduled":true},"text":"Refunds is ready: tell it what to refund."}`,
		// An improvement (kiff/tafs#107): the next version of a taf tafs runs, its Card
		// revision confirmed only at the switch; one that changes no Card has no draft.
		`{"type":"result","id":"v2","taf":{"agent":"refunds-k3f9","name":"Reembolsos","job":"refunds","improves":"taf-0123456789abcdef0123","note":"Lee el cobro antes de reembolsar.","notes":"Revisa cada cobro antes.","draft":"drf_02"},"text":"Listo: es su versión 2."}`,
		`{"type":"result","id":"v3","taf":{"agent":"refunds-k3f9","name":"Refunds","job":"refunds","improves":"taf-0123456789abcdef0123","note":"Asks before refunds over 50 EUR."},"text":"Ready as version 3."}`,
	} {
		r := call(t, in)
		if r.IsError || r.Component == nil {
			t.Fatalf("%s: %+v", in, r)
		}
		var got, want map[string]any
		_ = json.Unmarshal(r.Component, &got)
		_ = json.Unmarshal([]byte(in), &want)
		if g, _ := json.Marshal(got); string(g) != mustJSON(want) {
			t.Errorf("shown %s, want %s", g, mustJSON(want))
		}
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// What doesn't fit the contract comes back as an error the model can fix,
// and nothing is shown.
func TestWhatDoesNotFitIsRefused(t *testing.T) {
	long := strings.Repeat("x", show.MaxText+1)
	for in, want := range map[string]string{
		`{"type":"banner","id":"a"}`:                                                                                                               "type must be one of",
		`{"type":"notice","id":"Bad Id","tone":"info","text":"x"}`:                                                                                 "id must be",
		`{"type":"notice","id":"a","tone":"loud","text":"x"}`:                                                                                      "tone is one of",
		`{"type":"notice","id":"a","tone":"info","text":"` + long + `"}`:                                                                           "at most",
		`{"type":"notice","id":"a","tone":"info","text":"x","grant":"grt_1"}`:                                                                      `has no field "grant"`,
		`{"type":"notice","id":"a","tone":"info","text":"x","html":"<b>"}`:                                                                         "unknown",
		`{"type":"steps","id":"a","steps":[]}`:                                                                                                     "1 to 20 steps",
		`{"type":"steps","id":"a","steps":[{"label":"x","status":"maybe"}]}`:                                                                       "status is one of",
		`{"type":"confirm","id":"a","text":"?","options":[{"value":"y","label":"Y"}]}`:                                                             "2 to 4",
		`{"type":"approval","id":"a","grant":"grt 1"}`:                                                                                             "planque's grant id",
		`{"type":"approval","id":"a","grant":"grt_1","text":"Approve me"}`:                                                                         `has no field "text"`,
		`{"type":"approval","id":"a","grant":"grt_1","title":"Read only"}`:                                                                         `has no field "title"`,
		`{"type":"approval","id":"a","grant":"grt_1","yes":"Cancelar"}`:                                                                            `has no field "yes"`,
		`{"type":"approval","id":"a","grant":"grt_1","no":"Aprobar"}`:                                                                              `has no field "no"`,
		`{"type":"approval","id":"a","grant":"grt_1","note":"Solo puede leer"}`:                                                                    `has no field "note"`,
		`{"type":"sign_in","id":"a","grant":"grt_1","service":"x.test","yes":"` + strings.Repeat("y", 41) + `"}`:                                   "yes is 41 characters",
		`{"type":"confirm","id":"a","text":"?","options":[{"value":"y","label":"Y"},{"value":"n","label":"N"}],"yes":"Y"}`:                         `has no field "yes"`,
		`{"type":"sign_in","id":"a","grant":"grt_1"}`:                                                                                              "service is",
		`{"type":"result","id":"a","text":"done"}`:                                                                                                 "taf.agent",
		`{"type":"result","id":"a","taf":{"agent":"r","name":"R","draft":"drf_1"},"text":"done"}`:                                                  "taf.job",
		`{"type":"result","id":"a","taf":{"agent":"r","name":"R","job":"refunds"},"text":"done"}`:                                                  "taf.draft",
		`{"type":"result","id":"a","taf":{"agent":"r","name":"R","job":"refunds","draft":"drf_1","note":"x"},"text":"done"}`:                       "for an improvement",
		`{"type":"result","id":"a","taf":{"agent":"r","name":"R","job":"refunds","improves":"taf-1"},"text":"done"}`:                               "taf.note",
		`{"type":"result","id":"a","taf":{"agent":"r","name":"R","job":"refunds","improves":"taf-1","note":"n","unscheduled":true},"text":"done"}`: "for a new taf",
		`{"type":"questions","id":"q","text":"?","questions":[]}`:                                                                                  "1 to 4 questions",
		`{"type":"questions","id":"q","text":"?","questions":[{"id":"q0","text":"?","options":[{"value":"a","label":"A"},{"value":"b","label":"B"}]},{"id":"q1","text":"?","options":[{"value":"a","label":"A"},{"value":"b","label":"B"}]},{"id":"q2","text":"?","options":[{"value":"a","label":"A"},{"value":"b","label":"B"}]},{"id":"q3","text":"?","options":[{"value":"a","label":"A"},{"value":"b","label":"B"}]},{"id":"q4","text":"?","options":[{"value":"a","label":"A"},{"value":"b","label":"B"}]}]}`: "1 to 4 questions",
		`{"type":"questions","id":"q","text":"?","questions":[{"id":"x","text":"?","options":[{"value":"a","label":"A"},{"value":"b","label":"B"}]},{"id":"x","text":"?","options":[{"value":"a","label":"A"},{"value":"b","label":"B"}]}]}`:                                                                                                                                                                                                                                                                        "different",
		`{"type":"questions","id":"q","text":"?","questions":[{"id":"x","text":"?","options":[{"value":"a","label":"A"}]}]}`:                                      "2 to 5 options",
		`{"type":"questions","id":"q","text":"?","questions":[{"id":"x","text":"","options":[{"value":"a","label":"A"},{"value":"b","label":"B"}]}]}`:             "a question's text is required",
		`{"type":"questions","id":"q","text":"?","questions":[{"id":"x","text":"?","options":[{"value":"a","label":"A"},{"value":"b","label":"B"}],"own":true}]}`: "unknown",
		`{"type":"confirm","id":"a","text":"?","options":[{"value":"y","label":"Y","description":"more"},{"value":"n","label":"N"}]}`:                             "no description",
		`{"type":"questions","id":"q","questions":[{"id":"x","text":"?","options":[{"value":"a","label":"A"},{"value":"b","label":"B"}]}]}`:                       "text is required",
		`{"type":"questions","id":"q","text":"?","questions":[{"id":"cur","text":"?","options":[{"value":"usd","label":"USD"},{"value":"EUR","label":"EUR"}]}]}`:  `question cur: option 2 value "EUR": use 1 to 64 of a-z 0-9 - _`,
		`{"type":"confirm","id":"a","text":"?","options":[{"value":"y","label":"Y"},{"value":"y","label":"Again"}]}`:                                              `option 2 value "y": another option has it`,
		`{"type":"questions","id":"q","text":"?","questions":[{"id":"Currency","text":"?","options":[{"value":"a","label":"A"},{"value":"b","label":"B"}]}]}`:     `question 1 id "Currency": use 1 to 64`,
		`not json`: "a JSON object",
	} {
		r := call(t, in)
		if !r.IsError || r.Component != nil || !strings.Contains(r.Text, want) {
			t.Errorf("%s: %+v, want an error with %q", in, r, want)
		}
	}
}
