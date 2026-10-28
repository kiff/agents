package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/eval"
)

// The nightly log is public: neither the account nor the vendor's address
// may reach it, whole or as a bare host.
func TestTheLogHidesTheAccountAndTheVendor(t *testing.T) {
	var b bytes.Buffer
	out := &redacting{w: &b}
	out.hide("https://abc123.lambda-url.example/mcp", "<the vendor>/mcp")
	out.hide("abc123.lambda-url.example", "<the vendor>")
	out.hide("t_0123456789", "<the account>")
	fmt.Fprintln(out, "server https://abc123.lambda-url.example/mcp, sign in at abc123.lambda-url.example, account t_0123456789")
	if got, want := b.String(), "server <the vendor>/mcp, sign in at <the vendor>, account <the account>\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// In CI the runner masks each value too, and only the steps are written.
func TestInCIOnlyTheStepsAreWrittenAndEveryIdIsMasked(t *testing.T) {
	var b bytes.Buffer
	out := &redacting{w: &b, ci: true}
	out.hide("grt_abc", "<a grant>")
	tt := &test{out: out, cfg: config{log: out}}
	tt.logf("grant %s approved", "grt_abc")
	fmt.Fprintln(out, "FAIL: the grant grt_abc: 409")
	if got, want := b.String(), "::add-mask::grt_abc\nFAIL: the grant <a grant>: 409\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTafConcise(t *testing.T) {
	tt := &test{tafSaid: []string{"Refunded €3.00 to the customer's card.", "KIFF refused the €900 refund; nothing was sent."}}
	if err := tt.tafConcise(context.Background()); err != nil {
		t.Fatal(err)
	}
	// No word count fails an answer (kiff/agents#185): a long one passes without
	// -grade, and its count is logged.
	var counts strings.Builder
	tt = &test{cfg: config{log: &counts}, tafSaid: []string{"Done.", strings.Repeat("palabra ", 150)}}
	if err := tt.tafConcise(context.Background()); err != nil {
		t.Fatal(err)
	}
	if counts.String() != "taf  words: 1, 150\n" {
		t.Fatalf("logged %q", counts.String())
	}
	// With -grade the judge decides, and its reason stays out of the log.
	var log, transcript strings.Builder
	tt = &test{cfg: config{log: &log, transcript: &transcript, judge: &eval.Judge{Model: &verdicts{fail: []string{"concise"}}, ModelID: "judge"}},
		out: &redacting{w: &log, ci: true}, tafSaid: []string{"Great question! Let me tell you all about it."}}
	err := tt.tafConcise(context.Background())
	if err == nil || log.String() != "taf  words: 9\n" || !strings.Contains(transcript.String(), "not concise: judged") {
		t.Fatalf("err = %v, log %q, transcript %q", err, log.String(), transcript.String())
	}
}

func TestTafDefinitionFromTafs(t *testing.T) {
	f := filepath.Join(t.TempDir(), "refunds.json")
	os.WriteFile(f, []byte(`{"instructions":"You are a taf.","model":"claude-opus-5-5","effort":"medium","max_tokens":16000,"key_ref":"kiff/eval-refunds","max_cost_micro_usd":2000000,"history":10}`), 0o600)
	tt := &test{cfg: config{tafDef: f, maxCost: 4_000_000}, tenant: "t1"}
	d, err := tt.tafDefinition("refunds-x")
	if err != nil {
		t.Fatal(err)
	}
	if d.Instructions != "You are a taf." || d.MaxTokens != 16000 || d.Effort != "medium" {
		t.Fatalf("not tafs' definition: %+v", d)
	}
	if d.ID != "refunds-x" || d.KeyRef != "kiff/taf" || d.MaxCost != 1_000_000 || d.History != 2 || d.MaxTurns != 6 {
		t.Fatalf("the test's own agent, key and limits: %+v", d)
	}
}

func TestCoversTheJob(t *testing.T) {
	// "read" is the vendor's read for every operation, pinned to retrieving a
	// charge (kiff/tafs#174); "read anything" is the same tool left open.
	g := func(tools ...string) grant {
		var out grant
		for _, name := range tools {
			switch name {
			case "read":
				name = "stripe_api_read"
				out.Request.Tools = append(out.Request.Tools, grantTool{Tool: name, Fixed: map[string]any{"stripe_api_operation_id": "GetChargesCharge"}})
				continue
			case "read anything":
				name = "stripe_api_read"
			}
			out.Request.Tools = append(out.Request.Tools, grantTool{Tool: name})
		}
		return out
	}
	if err := coversTheJob(g("list_charges", "read", "create_refund")); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]grant{
		"only acts":              g("create_refund"),
		"can't check one":        g("list_charges", "create_refund"),
		"reads any operation":    g("list_charges", "read anything", "create_refund"),
		"pays out as well":       g("list_charges", "read", "create_refund", "payout_balance"),
		"the old dedicated read": g("list_charges", "retrieve_charge", "create_refund"),
	} {
		if coversTheJob(c) == nil {
			t.Errorf("%s: passed", name)
		}
	}
}

func TestReadThenRefunded(t *testing.T) {
	at := func(m int) time.Time { return time.Date(2026, 10, 20, 9, m, 0, 0, time.UTC) }
	var a activity
	add := func(actor, action, outcome string, m int) {
		a.Calls = append(a.Calls, struct {
			Actor   string    `json:"actor_id"`
			Action  string    `json:"action_name"`
			Outcome string    `json:"outcome"`
			At      time.Time `json:"created_at"`
		}{actor, action, outcome, at(m)})
	}
	add("reembolsos-x", "retrieve_charge_k3f9", "allowed", 1)
	add("reembolsos-x", "create_refund_k3f9", "allowed", 2)
	add("someone-else", "create_refund_zz", "allowed", 0)
	reads, refunds := []string{"retrieve_charge"}, []string{"create_refund"}
	if err := readThenRefunded(a.Calls, "reembolsos-x", reads, refunds); err != nil {
		t.Fatal(err)
	}
	refused := activity{Calls: append(a.Calls[:0:0], a.Calls...)}
	refused.Calls[0].Outcome = "blocked" // the read tool isn't on its Card
	if readThenRefunded(refused.Calls, "reembolsos-x", reads, refunds) == nil {
		t.Error("a refused read passed")
	}
	late := activity{Calls: append(a.Calls[:0:0], a.Calls...)}
	late.Calls[0].At = at(3) // it read only after refunding
	if readThenRefunded(late.Calls, "reembolsos-x", reads, refunds) == nil {
		t.Error("a read after the refund passed")
	}
	// The generic write, pinned to refunds, is the refund too (coversTheJob accepts it).
	generic := activity{Calls: append(a.Calls[:0:0], a.Calls...)}
	generic.Calls[1].Action = "stripe_api_write_k3f9"
	if err := readThenRefunded(generic.Calls, "reembolsos-x", reads, []string{"stripe_api_write"}); err != nil {
		t.Errorf("a read, then the generic write pinned to refunds: %v", err)
	}
	// Setup named the read (its unit_lookup's get_charge): KIFF records it by that
	// name, and it's still the read (main's Eval 39593151494 and 39601994170).
	named := activity{Calls: append(a.Calls[:0:0], a.Calls...)}
	named.Calls[0].Action = "get_charge_k3f9"
	if err := readThenRefunded(named.Calls, "reembolsos-x", []string{"get_charge"}, refunds); err != nil {
		t.Errorf("a read Setup named: %v", err)
	}
	err := readThenRefunded(named.Calls, "reembolsos-x", reads, refunds)
	if err == nil {
		t.Fatal("a call not named as the grant's read passed")
	}
	// The reason lists the taf's own calls in order, names and outcomes only.
	if !strings.Contains(err.Error(), "Its calls, in order: get_charge_k3f9 allowed; create_refund_k3f9 allowed") || strings.Contains(err.Error(), "someone-else") {
		t.Errorf("reason %q", err)
	}
}

// Setup connects the field-listing tool alone first (kiff/tafs#174): the job is
// covered by the grants together, as the e2e of 21 Oct read only that first one.
func TestAllToolsCoverTheJobAcrossGrants(t *testing.T) {
	var details, job grant
	details.Request.Tools = []grantTool{{Tool: "stripe_api_details", ReadOnly: true}}
	job.Request.Tools = []grantTool{{Tool: "list_charges"},
		{Tool: "stripe_api_read", Fixed: map[string]any{"stripe_api_operation_id": "GetChargesCharge"}}, {Tool: "create_refund"}}
	if coversTheJob(details) == nil {
		t.Fatal("the field-listing grant alone covered the job")
	}
	all := allTools([]grant{details, job})
	if err := coversTheJob(*all); err != nil || len(all.Request.Tools) != 4 {
		t.Fatalf("both grants: %v, %d tools", err, len(all.Request.Tools))
	}
}

// The account is pinned on every tool that takes it, reads included (kiff/tafs#138,
// item 58): the owner's taf looked it up on every message.
func TestPinsTheAccount(t *testing.T) {
	var g grant
	g.Request.Tools = []grantTool{{Tool: "list_charges"},
		{Tool: "stripe_api_read", Name: "get_charge", Fixed: map[string]any{"stripe_api_operation_id": "GetChargesCharge", "stripe_context": "acct_1"}},
		{Tool: "stripe_api_write", Name: "refund", Fixed: map[string]any{"stripe_api_operation_id": "PostRefunds", "stripe_context": "acct_1"}}}
	if err := pinsTheAccount(g); err != nil {
		t.Fatal(err)
	}
	g.Request.Tools[1].Fixed = map[string]any{"stripe_api_operation_id": "GetChargesCharge"}
	if err := pinsTheAccount(g); err == nil || !strings.Contains(err.Error(), "get_charge") {
		t.Fatalf("a read with the account open: %v", err)
	}
}

// The grant's own names for the charge's read and the refund: a tool's name
// when Setup gave one, else its vendor tool.
func TestChargeTools(t *testing.T) {
	var g grant
	g.Request.Tools = []grantTool{
		{Tool: "list_charges"},
		{Tool: "stripe_api_read", Name: "get_charge", ReadOnly: true, Fixed: map[string]any{"stripe_api_operation_id": "GetChargesCharge"}},
		{Tool: "stripe_api_read", Name: "get_payment", ReadOnly: true, Fixed: map[string]any{"stripe_api_operation_id": "GetPaymentIntentsIntent"}},
		{Tool: "create_refund"},
		{Tool: "stripe_api_write", Name: "refund", Fixed: map[string]any{"stripe_api_operation_id": "PostRefunds"}},
		{Tool: "stripe_api_write", Name: "payout", Fixed: map[string]any{"stripe_api_operation_id": "PostPayouts"}},
	}
	reads, refunds := chargeTools(g)
	if !slices.Equal(reads, []string{"get_charge"}) || !slices.Equal(refunds, []string{"create_refund", "refund"}) {
		t.Fatalf("reads %v, refunds %v", reads, refunds)
	}
}

// A listing read is found from the grant: no resource argument, and not the
// refund, the charge read or one of Setup's own reads, read-only or not.
func TestListTools(t *testing.T) {
	var g grant
	g.Request.Tools = []grantTool{
		{Tool: "list_charges", Name: "find_charges", ReadOnly: true},
		{Tool: "stripe_api_read", Name: "get_charge", ReadOnly: true, Fixed: map[string]any{"stripe_api_operation_id": "GetChargesCharge"}},
		{Tool: "retrieve_dispute", ReadOnly: true, ResourceArg: "dispute"},
		{Tool: "stripe_api_details", ReadOnly: true, SetupReads: true},
		{Tool: "search_payments"},
		{Tool: "create_refund"},
	}
	if got := listTools(g); !slices.Equal(got, []string{"find_charges", "search_payments"}) {
		t.Fatalf("listTools = %v", got)
	}
}

// The owner reads an approval in words: each tool by a title of its own, no
// tool name, grant prefix or host (kiff/tafs#138, items 52, 53).
func TestTheApprovalIsInWords(t *testing.T) {
	g := grant{Text: "You will sign in at mcp.example.com.\n\nThe taf may:\n• Refund a payment: moves money (amount), only within the taf's Card"}
	g.Request.Server = "https://mcp.example.com/mcp"
	g.Request.Tools = []grantTool{{Tool: "stripe_api_write", Name: "bxkwf7vi_refund_payment", Title: "Refund a payment"}}
	if raw := rawWords(g); raw != "" {
		t.Fatalf("words read as raw: %s", raw)
	}
	for text, want := range map[string]string{
		"The taf may:\n• bxkwf7vi_refund_payment: moves money": "the tool's name bxkwf7vi_refund_payment",
		"Connect mcp.example.com to the taf.":                  "the vendor's host mcp.example.com",
	} {
		g.Text = text
		if raw := rawWords(g); raw != want {
			t.Errorf("%q: %q, want %q", text, raw, want)
		}
	}
	g.Text, g.Request.Tools[0].Title = "The taf may:\n• Write Stripe API data", ""
	if raw := rawWords(g); raw != "Setup gave stripe_api_write no title" {
		t.Errorf("no title: %q", raw)
	}
	g.Request.Tools = []grantTool{{Tool: "stripe_api_write", Name: "a_refund", Title: "Refund a payment"}, {Tool: "stripe_api_write", Name: "a_cancel", Title: "refund a payment"}}
	if raw := rawWords(g); raw != `two tools titled "refund a payment"` {
		t.Errorf("one title twice: %q", raw)
	}
}

// The phone signs planque's requests as a KIFF device (d=), as the tafs app
// does, so planque can drop the key-enrolled path (kiff/planque#12).
func TestThePhoneSignsAsItsKIFFDevice(t *testing.T) {
	if h := signatureHeader("2026-10-20T09:00:00Z", []byte("sig"), "dev_1"); !strings.HasSuffix(h, ",d=dev_1") || !strings.HasPrefix(h, "t=2026-10-20T09:00:00Z,v1=") {
		t.Fatalf("header = %q", h)
	}
	if h := signatureHeader("2026-10-20T09:00:00Z", []byte("sig"), ""); strings.Contains(h, "d=") {
		t.Fatalf("header without a device = %q", h)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/me/devices" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"devices":[{"id":"dev_other","this_device":false},{"id":"dev_phone","this_device":true}]}`))
	}))
	defer srv.Close()
	got, err := thisDevice(context.Background(), &client{base: srv.URL, http: srv.Client(), auth: func(*http.Request, []byte) {}})
	if err != nil || got != "dev_phone" {
		t.Fatalf("thisDevice = %q, %v", got, err)
	}
	none := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"devices":[{"id":"dev_other","this_device":false}]}`))
	}))
	defer none.Close()
	if _, err := thisDevice(context.Background(), &client{base: none.URL, http: none.Client(), auth: func(*http.Request, []byte) {}}); err == nil {
		t.Fatal("a session with no device of its own passed")
	}
}

// Setup's reads go through KIFF (kiff/tafs#138, gate A): a read is on KIFF's
// record as Setup's call, allowed, with a receipt; anything else fails.
func TestSetupReadsOnRecord(t *testing.T) {
	receipt := &struct {
		TraceID string `json:"trace_id"`
	}{TraceID: "tr_1"}
	reads := []string{"list_accounts", "stripe_api_details"}
	for name, c := range map[string]struct {
		calls []setupCall
		ok    bool
	}{
		"one read, allowed, with its receipt": {[]setupCall{{Actor: "setup", Action: "list_accounts", Outcome: "allowed", Receipt: receipt}}, true},
		"no read on the record":               {nil, false},
		"another agent's read":                {[]setupCall{{Actor: "taf", Action: "list_accounts", Outcome: "allowed", Receipt: receipt}}, false},
		"a tool that isn't a Setup read":      {[]setupCall{{Actor: "setup", Action: "create_refund", Outcome: "allowed", Receipt: receipt}}, false},
		"refused":                             {[]setupCall{{Actor: "setup", Action: "list_accounts", Outcome: "blocked", Receipt: receipt}}, false},
		"no receipt":                          {[]setupCall{{Actor: "setup", Action: "list_accounts", Outcome: "allowed"}}, false},
	} {
		if err := readsOnRecord(c.calls, "setup", reads); (err == nil) != c.ok {
			t.Errorf("%s: %v, want ok=%v", name, err, c.ok)
		}
	}
	g := grant{}
	g.Request.Tools = []grantTool{{Tool: "list_accounts", SetupReads: true}, {Tool: "stripe_api_details", Name: "details", SetupReads: true}, {Tool: "create_refund"}}
	if got := setupReadTools(g); !slices.Equal(got, []string{"list_accounts", "details"}) {
		t.Errorf("setupReadTools: %v", got)
	}
}

// An improvement adds to the taf's connection, never opens another (kiff/tafs#138).
func TestAnImprovementOpensNoConnection(t *testing.T) {
	if err := sameConnections(1, 1); err != nil {
		t.Fatal(err)
	}
	if err := sameConnections(1, 3); err == nil || !strings.Contains(err.Error(), "opened 2 new connection(s)") {
		t.Fatalf("err = %v", err)
	}
}

// Archiving a taf is a signed setup call from the phone's session
// (kiff/agents#78); revoking a Card isn't signed.
func TestArchivingATafIsSigned(t *testing.T) {
	for _, c := range []struct {
		method, path string
		signed       bool
	}{
		{http.MethodPost, "/v1/me/agents/taf-x7k3/archive", true},
		{http.MethodPost, "/v1/me/agent-drafts/drf_1/confirm", true},
		{http.MethodPost, "/v1/me/mandates/m_1/revoke", false},
		{http.MethodGet, "/v1/me/agents/taf-x7k3/archive", false},
		{http.MethodPost, "/v1/agents/taf-x7k3/grants", true},
		{http.MethodDelete, "/v1/agents/taf-x7k3/grants", true},
		{http.MethodGet, "/v1/agents/taf-x7k3/grants", false},
	} {
		if got := signedPath(c.method, c.path); got != c.signed {
			t.Errorf("%s %s: signed %v, want %v", c.method, c.path, got, c.signed)
		}
	}
}

// An account's grants list grows with every run: a list over a megabyte is
// read whole, so a grant at its end is found, and one cut short or not
// JSON is an error, never an empty list (kiff/tafs#214).
func TestPhoneReadsALongGrantsList(t *testing.T) {
	var gs []string
	for i := range 3000 {
		gs = append(gs, fmt.Sprintf(`{"id":"grt_%04d","status":"disconnected","text":"%s"}`, i, strings.Repeat("x", 400)))
	}
	list := `{"grants":[` + strings.Join(gs, ",") + `]}`
	body := list
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
	defer srv.Close()
	p := &phone{planque: &client{base: srv.URL, http: srv.Client()}, tenant: "t1"}
	if len(list) < 1<<20 {
		t.Fatalf("the list is only %d bytes", len(list))
	}
	if g, err := p.grant(context.Background(), "grt_2999"); err != nil || g.ID != "grt_2999" {
		t.Fatalf("the last grant of a long list: %+v %v", g, err)
	}
	body = list[:len(list)/2]
	if _, err := p.grants(context.Background()); err == nil {
		t.Fatal("a list cut short read as a list")
	}
}

// fakeShared is planque and KIFF on an account the test shares with its
// owner: the owner's Refunds grant (grt_owner) and Card (m_owner), and an
// earlier run's grant (grt_old), asked for by the test's setup agent. It
// records each call that changes something.
func fakeShared(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var changed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/accounts/t1/asked-grants":
			if r.Header.Get("Authorization") != "Bearer setup-key" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"grants":[{"id":"grt_old","status":"connected","tools":[{"name":"create_refund"}]}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/accounts/t1/grants":
			// The account's whole list, the owner's included.
			_, _ = w.Write([]byte(`{"grants":[{"id":"grt_owner","status":"connected"},{"id":"grt_old","status":"connected"}]}`))
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
		default:
			changed = append(changed, r.Method+" "+r.URL.Path)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &changed
}

// A run touches only what it made: at its start it disconnects only the
// grants its own setup agent asked for, and its cleanup only what it
// recorded. The owner's connected grant and Card on the same account are
// left alone.
func TestARunLeavesWhatItDidntMake(t *testing.T) {
	srv, changed := fakeShared(t)
	var log bytes.Buffer
	tt := &test{cfg: config{planque: srv.URL, setupKey: "setup-key", log: &log}, out: &redacting{w: &log}, hc: srv.Client(),
		phone: &phone{planque: &client{base: srv.URL, http: srv.Client()}, tenant: "t1"},
		kiff:  &client{base: srv.URL, http: srv.Client()}}
	if err := tt.clearEarlierRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"DELETE /v1/accounts/t1/grants/grt_old"}; !slices.Equal(*changed, want) {
		t.Fatalf("at the start: %v, want %v", *changed, want)
	}
	*changed = nil
	tt.grants, tt.cardIDs, tt.tafAgents = []string{"grt_new"}, []string{"m_new"}, []string{"taf-new"}
	tt.cleanup()
	want := []string{"DELETE /v1/accounts/t1/grants/grt_new", "POST /v1/me/mandates/m_new/revoke", "POST /v1/me/agents/taf-new/archive"}
	if !slices.Equal(*changed, want) {
		t.Fatalf("at cleanup: %v, want %v", *changed, want)
	}
	for _, c := range *changed {
		if strings.Contains(c, "owner") {
			t.Fatalf("touched the owner's %s", c)
		}
	}
}

// In CI, a judged fail's reason stays out of the public log, and the
// private transcript has it, with every text the judges read in full.
func TestAJudgedFailsReasonIsInTheTranscript(t *testing.T) {
	long := "Pásale cada reembolso con " + strings.Repeat("el cobro ", 40) + "y su id ch_…"
	var log, transcript bytes.Buffer
	tt := &test{out: &redacting{w: &log, ci: true}, said: said{messages: []string{"Hola.", long}},
		cfg: config{log: &log, transcript: &transcript, judge: &eval.Judge{}}}
	err := tt.reportSetup([]eval.CheckResult{
		{Name: "concise", Grade: eval.Grade{Pass: true}},
		{Name: "asks for no id", Grade: eval.Grade{Why: "message 2 asks the owner for a charge id"}},
	})
	if err == nil || !strings.Contains(err.Error(), "asks for no id") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(log.String(), "charge id") || strings.Contains(log.String(), "Pásale") {
		t.Fatalf("the public log has the chat: %q", log.String())
	}
	got := transcript.String()
	if !strings.Contains(got, "FAIL asks for no id: message 2 asks the owner for a charge id") || !strings.Contains(got, long) {
		t.Fatalf("the transcript: %q", got)
	}
}
