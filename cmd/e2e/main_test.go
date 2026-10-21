package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestTafBrief(t *testing.T) {
	if err := tafBrief([]string{"Refunded €3.00 to the customer's card.", "KIFF refused the €900 refund; nothing was sent."}); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("word ", maxTafWords+1)
	err := tafBrief([]string{"Done.", long})
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("answer 2 has %d words", maxTafWords+1)) {
		t.Fatalf("err = %v", err)
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
	g := func(tools ...string) grant {
		var out grant
		for _, name := range tools {
			out.Request.Tools = append(out.Request.Tools, grantTool{Tool: name})
		}
		return out
	}
	if err := coversTheJob(g("list_charges", "retrieve_charge", "create_refund")); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]grant{
		"only acts":        g("create_refund"),
		"can't check one":  g("list_charges", "create_refund"),
		"pays out as well": g("list_charges", "retrieve_charge", "create_refund", "payout_balance"),
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
	if err := readThenRefunded(a.Calls, "reembolsos-x"); err != nil {
		t.Fatal(err)
	}
	refused := activity{Calls: append(a.Calls[:0:0], a.Calls...)}
	refused.Calls[0].Outcome = "blocked" // the read tool isn't on its Card
	if readThenRefunded(refused.Calls, "reembolsos-x") == nil {
		t.Error("a refused read passed")
	}
	late := activity{Calls: append(a.Calls[:0:0], a.Calls...)}
	late.Calls[0].At = at(3) // it read only after refunding
	if readThenRefunded(late.Calls, "reembolsos-x") == nil {
		t.Error("a read after the refund passed")
	}
	// The generic write, pinned to refunds, is the refund too (coversTheJob accepts it).
	generic := activity{Calls: append(a.Calls[:0:0], a.Calls...)}
	generic.Calls[1].Action = "stripe_api_write_k3f9"
	if err := readThenRefunded(generic.Calls, "reembolsos-x"); err != nil {
		t.Errorf("a read, then the generic write pinned to refunds: %v", err)
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
