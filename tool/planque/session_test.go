package planque_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/tool/planque"
)

// Every request that makes a grant carries the chat that asks for it, as the
// runner names it, so a chat's delete can end what it asked for (planque's
// session agents). Never a session the model writes: a "session" in its
// input is ignored, and with no run nothing is added.
func TestARequestCarriesTheRunnersSessionNeverTheModels(t *testing.T) {
	srv, seen := fake(t, true)
	tl := planque.New(srv.URL+"/", "tnt_1", "k_setup", nil)
	ctx := run.WithRef(context.Background(), run.Ref{Session: "ses_chat01", Run: "run_1"})
	sent := func(in string) map[string]any {
		t.Helper()
		if _, err := tl.Call(ctx, json.RawMessage(in)); err != nil {
			t.Fatal(err)
		}
		seenMu.Lock()
		last := (*seen)[len(*seen)-1]
		seenMu.Unlock()
		var body map[string]any
		if err := json.Unmarshal([]byte(last[strings.Index(last, "{"):]), &body); err != nil {
			t.Fatalf("%s: %v", last, err)
		}
		return body
	}
	tools := `"tools":[{"tool":"list_charges","title":"List charges","name":"list_charges","read_only":true}]`
	for name, in := range map[string]string{
		"by server": `{"action":"request","server":"https://mcp.stripe.com","agent":"refunds-a1",` + tools + `,"session":"ses_model"}`,
		"by entry":  `{"action":"request","entry":"connector:composio:quickbooks","agent":"invoices-a1","session":"ses_model"}`,
		"reuse":     `{"action":"request","reuse":"grt_old","agent":"refunds-a1","tools":[],"session":"ses_model"}`,
		"browser": `{"action":"request","entry":"` + planque.BrowserEntry + `","agent":"shop-a1","browser":{"sites":["shop.example"]},` +
			`"tools":[{"tool":"browse_open","title":"Open a page"}],"session":"ses_model"}`,
	} {
		if got := sent(in)["session"]; got != "ses_chat01" {
			t.Errorf("%s: session %v, want the runner's", name, got)
		}
	}

	// With no run, nothing is added; neither is a session planque wouldn't take.
	if _, err := tl.Call(context.Background(), json.RawMessage(`{"action":"request","entry":"connector:composio:quickbooks","agent":"invoices-a1"}`)); err != nil {
		t.Fatal(err)
	}
	seenMu.Lock()
	last := (*seen)[len(*seen)-1]
	seenMu.Unlock()
	if strings.Contains(last, `"session"`) {
		t.Errorf("a request with no run carries a session: %s", last)
	}
	odd := run.WithRef(context.Background(), run.Ref{Session: "not an id", Run: "run_2"})
	if _, err := tl.Call(odd, json.RawMessage(`{"action":"request","entry":"connector:composio:quickbooks","agent":"invoices-a1"}`)); err != nil {
		t.Fatal(err)
	}
	seenMu.Lock()
	last = (*seen)[len(*seen)-1]
	seenMu.Unlock()
	if strings.Contains(last, `"session"`) {
		t.Errorf("a session planque wouldn't take was sent: %s", last)
	}
}
