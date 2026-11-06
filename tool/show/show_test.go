package show_test

import (
	"context"
	"encoding/json"
	"fmt"
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
		`{"type":"result","id":"listo","taf":{"agent":"refunds","name":"Reembolsos","draft":"drf_01","kickoff":"Work the refund queue.","cron":"*/30 * * * *"},"text":"Listo.","yes":"Ver Reembolsos"}`,
		`{"type":"result","id":"done","taf":{"agent":"refunds","name":"Refunds","job":"custom","draft":"drf_01","kickoff":"Work the refund queue.","cron":"*/30 * * * *","instructions":"Never refund twice."},"text":"Refunds is ready.","tools":["stripe_refund"]}`,
		// Its tools can't find its work: no schedule, the owner hands it work (kiff/tafs#138).
		`{"type":"result","id":"chat","taf":{"agent":"refunds","name":"Refunds","draft":"drf_01","unscheduled":true,"kickoff":"Refund what the owner hands you."},"text":"Refunds is ready: tell it what to refund."}`,
		// An improvement (kiff/tafs#107): the next version of a taf tafs runs, its Card
		// revision confirmed only at the switch; one that changes no Card has no draft.
		`{"type":"result","id":"v2","taf":{"agent":"refunds-k3f9","name":"Reembolsos","job":"refunds","improves":"taf-0123456789abcdef0123","note":"Lee el cobro antes de reembolsar.","notes":"Revisa cada cobro antes.","draft":"drf_02"},"text":"Listo: es su versión 2."}`,
		`{"type":"result","id":"v3","taf":{"agent":"refunds-k3f9","name":"Refunds","job":"refunds","improves":"taf-0123456789abcdef0123","note":"Asks before refunds over 50 EUR."},"text":"Ready as version 3."}`,
		`{"type":"result","id":"v4","taf":{"agent":"refunds-k3f9","name":"Refunds","job":"refunds","improves":"taf-0123456789abcdef0123","note":"Offers a free month first.","summary":"Offers a free month to customers who cancel."},"text":"Ready as version 4."}`,
		`{"type":"result","id":"new","taf":{"agent":"refunds","name":"Refunds","draft":"drf_01","summary":"Answers refund requests within its Card.","kickoff":"Work the refund queue.","cron":"*/30 * * * *"},"text":"Refunds is ready."}`,
		// The dial version Coach drafted: the app draws it from tafs cloud's words.
		`{"type":"knob","id":"dial-v8","taf_id":"taf-0123456789abcdef0123","version":8}`,
	} {
		r := call(t, in)
		if r.IsError || r.Component == nil {
			t.Fatalf("%s: %+v", in, r)
		}
		var got, want map[string]any
		_ = json.Unmarshal(r.Component, &got)
		_ = json.Unmarshal([]byte(in), &want)
		// The card as the agent wrote it, and who its run waits on, which the tool adds.
		waits, _ := got["waits"].(string)
		delete(got, "waits")
		if g, _ := json.Marshal(got); string(g) != mustJSON(want) {
			t.Errorf("shown %s, want %s", g, mustJSON(want))
		}
		if w := map[string]string{"confirm": "owner", "questions": "owner", "approval": "owner", "sign_in": "owner", "drive": "owner", "result": "service"}[want["type"].(string)]; waits != w {
			t.Errorf("%s waits %q, want %q", in, waits, w)
		}
	}
}

// A shown card says whether its run waits on it, and on whom, from the one
// list where the run pauses (Answerable, ByService): the owner for the cards
// they answer, the service for its hand-over, and nothing for a card the run
// goes on past (steps, notice, knob). The agent can't write it (kiff/tafs: the
// apps and tafs cloud kept lists of their own, which drifted).
func TestACardSaysWhoItsRunWaitsOn(t *testing.T) {
	r := call(t, `{"type":"drive","id":"d1","text":"Show where Refunds' daily limit is set"}`)
	if r.IsError || !strings.Contains(string(r.Component), `"waits":"owner"`) {
		t.Fatalf("a drive card: %+v %s", r, r.Component)
	}
	r = call(t, `{"type":"knob","id":"dial-v8","taf_id":"taf-0123456789abcdef0123","version":8}`)
	if r.IsError || strings.Contains(string(r.Component), `"waits"`) {
		t.Fatalf("a knob card: %+v %s", r, r.Component)
	}
	// The agent's own "waits" is refused, as any field its type doesn't take.
	r = call(t, `{"type":"notice","id":"n1","text":"Done.","waits":"owner"}`)
	if !r.IsError || !strings.Contains(r.Text, `no field "waits"`) {
		t.Fatalf("an agent's waits: %+v", r)
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
		`{"type":"banner","id":"a"}`:                                                                                       "type must be one of",
		`{"type":"notice","id":"Bad Id","tone":"info","text":"x"}`:                                                         "id must be",
		`{"type":"notice","id":"a","tone":"loud","text":"x"}`:                                                              "tone is one of",
		`{"type":"notice","id":"a","tone":"info","text":"` + long + `"}`:                                                   "at most",
		`{"type":"notice","id":"a","tone":"info","text":"x","grant":"grt_1"}`:                                              `has no field "grant"`,
		`{"type":"notice","id":"a","tone":"info","text":"x","html":"<b>"}`:                                                 "unknown",
		`{"type":"steps","id":"a","steps":[]}`:                                                                             "1 to 20 steps",
		`{"type":"steps","id":"a","steps":[{"label":"x","status":"maybe"}]}`:                                               "status is one of",
		`{"type":"confirm","id":"a","text":"?","options":[{"value":"y","label":"Y"}]}`:                                     "2 to 4",
		`{"type":"approval","id":"a","grant":"grt 1"}`:                                                                     "planque's grant id",
		`{"type":"approval","id":"a","grant":"grt_1","text":"Approve me"}`:                                                 `has no field "text"`,
		`{"type":"approval","id":"a","grant":"grt_1","title":"Read only"}`:                                                 `has no field "title"`,
		`{"type":"approval","id":"a","grant":"grt_1","yes":"Cancelar"}`:                                                    `has no field "yes"`,
		`{"type":"approval","id":"a","grant":"grt_1","no":"Aprobar"}`:                                                      `has no field "no"`,
		`{"type":"approval","id":"a","grant":"grt_1","note":"Solo puede leer"}`:                                            `has no field "note"`,
		`{"type":"sign_in","id":"a","grant":"grt_1","service":"x.test","yes":"` + strings.Repeat("y", 41) + `"}`:           "yes is 41 characters",
		`{"type":"confirm","id":"a","text":"?","options":[{"value":"y","label":"Y"},{"value":"n","label":"N"}],"yes":"Y"}`: `has no field "yes"`,
		`{"type":"sign_in","id":"a","grant":"grt_1"}`:                                                                      "service is",
		`{"type":"result","id":"a","text":"done"}`:                                                                         "taf.agent",
		`{"type":"result","id":"a","taf":{"agent":"r","name":"R","draft":"drf_1"},"text":"done"}`:                          "taf.kickoff",
		`{"type":"result","id":"a","taf":{"agent":"r","name":"R","job":"billpay","draft":"drf_1","kickoff":"Work the refund queue.","cron":"*/30 * * * *"},"text":"done"}`:                                          "leave taf.job out",
		`{"type":"result","id":"a","taf":{"agent":"r","name":"R","draft":"drf_1","kickoff":"k"},"text":"done"}`:                                                                                                     "taf.cron",
		`{"type":"result","id":"a","taf":{"agent":"r","name":"R","draft":"drf_1","kickoff":"k","cron":"every morning"},"text":"done"}`:                                                                              "taf.cron",
		`{"type":"result","id":"a","taf":{"agent":"r","name":"R","draft":"drf_1","kickoff":"k","cron":"0 9 * * 1-5","unscheduled":true},"text":"done"}`:                                                             "an unscheduled one",
		`{"type":"result","id":"a","taf":{"agent":"r","name":"R","draft":"drf_1","kickoff":"` + strings.Repeat("k", show.MaxJob+1) + `","cron":"0 9 * * 1-5"},"text":"done"}`:                                       "taf.kickoff",
		`{"type":"result","id":"a","taf":{"agent":"r","name":"R","job":"refunds","improves":"taf-1","note":"n","cron":"0 9 * * 1-5"},"text":"done"}`:                                                                "for a new taf",
		`{"type":"result","id":"a","taf":{"agent":"r","name":"R","improves":"taf-1","note":"n"},"text":"done"}`:                                                                                                     "taf.job",
		`{"type":"result","id":"a","taf":{"agent":"r","name":"R"},"text":"done"}`:                                                                                                                                   "taf.draft",
		`{"type":"result","id":"a","taf":{"agent":"r","name":"R","draft":"drf_1","note":"x"},"text":"done"}`:                                                                                                        "for an improvement",
		`{"type":"result","id":"a","taf":{"agent":"r","name":"R","job":"refunds","improves":"taf-1"},"text":"done"}`:                                                                                                "taf.note",
		`{"type":"result","id":"a","taf":{"agent":"r","name":"R","job":"refunds","improves":"taf-1","note":"n","unscheduled":true},"text":"done"}`:                                                                  "for a new taf",
		`{"type":"result","id":"a","taf":{"agent":"r","name":"R","draft":"drf_1","kickoff":"Work the refund queue.","cron":"*/30 * * * *","summary":"` + strings.Repeat("s", show.MaxLabel+1) + `"},"text":"done"}`: "taf.summary",
		`{"type":"knob","id":"d","version":8}`:                                     "taf_id is",
		`{"type":"knob","id":"d","taf_id":"taf-1"}`:                                "version is",
		`{"type":"knob","id":"d","taf_id":"taf-1","version":8,"text":"Runs at 9"}`: `has no field "text"`,
		`{"type":"questions","id":"q","text":"?","questions":[]}`:                  "1 to 4 questions",
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

// A step that names its call is done only once the call went through
// (agents#293): refused, held, not made or failed is refused, with why;
// failed and skipped stay allowed, as does a call the run didn't make.
func TestAStepIsDoneOnlyOnceItsCallWentThrough(t *testing.T) {
	latest := map[string]string{
		"kiff__desk__post_reply":  "refused by KIFF, not sent",
		"planque_connect":         "done",
		"kiff__desk__apply_label": "held by KIFF, approved by the owner, then sent once: done",
		"search_tickets":          "failed",
	}
	ctx := tool.WithOutcomes(context.Background(), func(name string) string {
		if oc, ok := latest[name]; ok {
			return oc
		}
		for full, oc := range latest {
			if strings.HasSuffix(full, "__"+name) {
				return oc
			}
		}
		return ""
	})
	plan := func(status, call string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"type":"steps","id":"plan","steps":[{"label":"Reply to the customer","status":%q,"call":%q}]}`, status, call))
	}
	for _, tt := range []struct {
		status, call string
		refused      bool
	}{
		{"done", "kiff__desk__post_reply", true},
		{"done", "post_reply", true}, // by its operation alone
		{"done", "search_tickets", true},
		{"failed", "kiff__desk__post_reply", false},
		{"skipped", "kiff__desk__post_reply", false},
		{"done", "planque_connect", false},
		{"done", "apply_label", false},  // approved by the owner, then sent
		{"done", "fetch_ticket", false}, // not made in this run
		{"done", "", false},
	} {
		r, err := (show.Tool{}).Call(ctx, plan(tt.status, tt.call))
		if err != nil {
			t.Fatal(err)
		}
		if r.IsError != tt.refused {
			t.Errorf("%s %q: refused=%v, %s", tt.status, tt.call, r.IsError, r.Text)
		}
		if tt.refused && !strings.Contains(r.Text, "Mark it failed or skipped") {
			t.Errorf("%s %q: says %q", tt.status, tt.call, r.Text)
		}
	}
}

// A new taf's result names its tools that write outside the company, with
// their free-text fields: tafs gives the taf its AI disclosure there
// (kiff/tafs#476, gap 9). What doesn't fit is refused in words for Coach.
func TestAResultNamesWhereItsDisclosureGoes(t *testing.T) {
	ok := call(t, `{"type":"result","id":"done","taf":{"agent":"invoices","name":"Invoices","draft":"drf_01","kickoff":"Remind each customer.","cron":"0 9 * * 1-5",`+
		`"disclose":[{"tool":"send_message_k3f9","fields":["text"]}]},"text":"Invoices is ready."}`)
	if ok.IsError || !strings.Contains(string(ok.Component), `"disclose":[{"tool":"send_message_k3f9","fields":["text"]}]`) {
		t.Fatalf("a result with its disclosure: %+v", ok)
	}
	for name, taf := range map[string]string{
		"a tool with no name":               `"disclose":[{"tool":"","fields":["text"]}]`,
		"no field":                          `"disclose":[{"tool":"send","fields":[]}]`,
		"a path with a gap":                 `"disclose":[{"tool":"send","fields":["a..b"]}]`,
		"an improvement's, without a field": `"improves":"taf-1","job":"refunds","note":"n","disclose":[{"tool":"send","fields":[]}]`,
	} {
		in := `{"type":"result","id":"done","taf":{"agent":"invoices","name":"Invoices","draft":"drf_01","kickoff":"Remind.","cron":"0 9 * * 1-5",` + taf + `},"text":"t"}`
		if strings.Contains(taf, "improves") {
			in = `{"type":"result","id":"done","taf":{"agent":"invoices","name":"Invoices",` + taf + `},"text":"t"}`
		}
		if r := call(t, in); !r.IsError || !strings.Contains(r.Text, "disclose") {
			t.Errorf("%s: %+v", name, r)
		}
	}
	// An improvement may add one: for a tool it adds, or a taf from before (agentgabo on #354).
	more := call(t, `{"type":"result","id":"better","taf":{"agent":"invoices","name":"Invoices","improves":"taf-1","job":"custom","note":"Also replies",`+
		`"disclose":[{"tool":"reply_k3f9","fields":["body"]}]},"text":"t"}`)
	if more.IsError || !strings.Contains(string(more.Component), `"disclose":[{"tool":"reply_k3f9","fields":["body"]}]`) {
		t.Fatalf("an improvement's disclosure: %+v", more)
	}
}
