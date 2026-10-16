package stripemcp_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kiff/agents/eval/stripemcp"
)

const (
	testKey = "sk_test_fakefakefakefakefake"
	token   = "tok_0123456789abcdef0123456789abcdef"
)

// fakeStripe holds one dispute, dp_1, for 12000 cents, and records each
// write it was sent.
type fakeStripe struct {
	srv    *httptest.Server
	writes []string // method path form
	status string
}

func newStripe(t *testing.T) *fakeStripe {
	t.Helper()
	f := &fakeStripe{status: "needs_response"}
	dispute := func() map[string]any {
		return map[string]any{"id": "du_1", "amount": 12000, "currency": "usd", "status": f.status, "reason": "product_not_received",
			"evidence":         map[string]any{"shipping_tracking_number": "", "uncategorized_text": nil},
			"evidence_details": map[string]any{"due_by": 1760659200},
			"payment_intent":   map[string]any{"id": "pi_1", "metadata": map[string]any{"order": "T-2001", "delivery": "signed"}}}
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, _, _ := r.BasicAuth(); u != testKey {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid API Key provided: ` + u + `"}}`))
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/disputes":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{dispute()}})
		case r.Method == "GET" && r.URL.Path == "/v1/disputes/du_1":
			_ = json.NewEncoder(w).Encode(dispute())
		case r.Method == "GET":
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"message":"No such dispute"}}`))
		default:
			_ = r.ParseForm()
			f.writes = append(f.writes, r.Method+" "+r.URL.Path+" "+r.PostForm.Encode())
			if strings.HasSuffix(r.URL.Path, "/close") {
				f.status = "lost"
			}
			_ = json.NewEncoder(w).Encode(dispute())
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func server(t *testing.T, f *fakeStripe) *httptest.Server {
	t.Helper()
	s, err := stripemcp.New(testKey, token)
	if err != nil {
		t.Fatal(err)
	}
	s.Base = f.srv.URL
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv
}

// rpc posts a JSON-RPC request with the bearer token and decodes the answer.
func rpc(t *testing.T, srv *httptest.Server, bearer, method string, params any) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// call calls a tool and returns its text and whether it is an error.
func call(t *testing.T, srv *httptest.Server, name string, args map[string]any) (string, bool) {
	t.Helper()
	code, out := rpc(t, srv, token, "tools/call", map[string]any{"name": name, "arguments": args})
	if code != 200 || out["result"] == nil {
		t.Fatalf("%s: %d %v", name, code, out)
	}
	res := out["result"].(map[string]any)
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	isErr, _ := res["isError"].(bool)
	return text, isErr
}

func TestSpeaksMCPToItsCallerOnly(t *testing.T) {
	srv := server(t, newStripe(t))
	for _, bearer := range []string{"", "tok_wrong"} {
		if code, _ := rpc(t, srv, bearer, "tools/list", nil); code != 401 {
			t.Fatalf("bearer %q: %d, want 401", bearer, code)
		}
	}
	code, out := rpc(t, srv, token, "initialize", map[string]any{"protocolVersion": stripemcp.ProtocolVersion})
	if code != 200 || out["result"].(map[string]any)["protocolVersion"] != stripemcp.ProtocolVersion {
		t.Fatalf("initialize: %d %v", code, out)
	}
	_, out = rpc(t, srv, token, "tools/list", nil)
	tools := out["result"].(map[string]any)["tools"].([]any)
	names := map[string]map[string]any{}
	for _, tl := range tools {
		m := tl.(map[string]any)
		names[m["name"].(string)] = m
	}
	for _, n := range []string{"list_disputes", "retrieve_dispute", "update_dispute", "close_dispute"} {
		if names[n] == nil {
			t.Fatalf("no %s in %v", n, names)
		}
	}
	for _, n := range []string{"update_dispute", "close_dispute"} { // KIFF reads a Card's amount from an integer argument
		amount := names[n]["inputSchema"].(map[string]any)["properties"].(map[string]any)["amount"].(map[string]any)
		if amount["type"] != "integer" {
			t.Fatalf("%s's amount is %v, want an integer", n, amount)
		}
	}
	// A notification gets 202 and no body.
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	if resp, _ := srv.Client().Do(req); resp.StatusCode != 202 {
		t.Fatalf("a notification: %d", resp.StatusCode)
	}
}

func TestReadsDisputes(t *testing.T) {
	srv := server(t, newStripe(t))
	text, isErr := call(t, srv, "list_disputes", map[string]any{"status": "needs_response"})
	if isErr || !strings.Contains(text, `"id":"du_1"`) || !strings.Contains(text, `"amount":12000`) || !strings.Contains(text, `"order":"T-2001"`) {
		t.Fatalf("list: %s", text)
	}
	if text, _ := call(t, srv, "list_disputes", map[string]any{"status": "won"}); strings.Contains(text, "du_1") {
		t.Fatalf("filtered by status: %s", text)
	}
	text, isErr = call(t, srv, "retrieve_dispute", map[string]any{"dispute": "du_1"})
	if isErr || !strings.Contains(text, `"delivery":"signed"`) || !strings.Contains(text, `"evidence_due_by":"2025-10-17T00:00:00Z"`) {
		t.Fatalf("retrieve: %s", text)
	}
}

// A write goes to Stripe only with the dispute's own amount: another amount
// is refused, and nothing is sent.
func TestAWriteNeedsTheDisputesAmount(t *testing.T) {
	f := newStripe(t)
	srv := server(t, f)
	for _, amount := range []int{11999, 0, 1} {
		for _, tool := range []string{"update_dispute", "close_dispute"} {
			text, isErr := call(t, srv, tool, map[string]any{"dispute": "du_1", "amount": amount, "evidence": map[string]string{"uncategorized_text": "x"}})
			if !isErr || !strings.Contains(text, "isn't the dispute's amount") {
				t.Fatalf("%s with %d: %s", tool, amount, text)
			}
		}
	}
	if text, isErr := call(t, srv, "close_dispute", map[string]any{"dispute": "du_1"}); !isErr || !strings.Contains(text, "amount") {
		t.Fatalf("no amount: %s", text)
	}
	if len(f.writes) != 0 {
		t.Fatalf("sent %v", f.writes)
	}

	text, isErr := call(t, srv, "update_dispute", map[string]any{"dispute": "du_1", "amount": 12000, "submit": true,
		"evidence": map[string]string{"shipping_tracking_number": "1Z999", "uncategorized_text": "Signed for on delivery."}})
	if isErr || len(f.writes) != 1 {
		t.Fatalf("update: %s, writes %v", text, f.writes)
	}
	form, _ := url.ParseQuery(strings.SplitN(f.writes[0], " ", 3)[2])
	if !strings.HasPrefix(f.writes[0], "POST /v1/disputes/du_1 ") || form.Get("evidence[shipping_tracking_number]") != "1Z999" || form.Get("submit") != "true" {
		t.Fatalf("sent %v", f.writes[0])
	}
	if text, isErr := call(t, srv, "close_dispute", map[string]any{"dispute": "du_1", "amount": 12000}); isErr || !strings.HasPrefix(f.writes[1], "POST /v1/disputes/du_1/close") || !strings.Contains(text, `"status":"lost"`) {
		t.Fatalf("close: %s, writes %v", text, f.writes)
	}
}

func TestRefuses(t *testing.T) {
	f := newStripe(t)
	srv := server(t, f)
	for name, c := range map[string]struct {
		tool string
		args map[string]any
		want string
	}{
		"evidence it doesn't take": {"update_dispute", map[string]any{"dispute": "du_1", "amount": 12000, "evidence": map[string]string{"receipt": "file_1"}}, "no field"},
		"not a dispute id":         {"retrieve_dispute", map[string]any{"dispute": "../charges/ch_1"}, "dispute id"},
		"an unknown dispute":       {"retrieve_dispute", map[string]any{"dispute": "du_9"}, "No such dispute"},
		"an unknown tool":          {"create_payout", map[string]any{}, "no tool"},
	} {
		if text, isErr := call(t, srv, c.tool, c.args); !isErr || !strings.Contains(text, c.want) {
			t.Errorf("%s: %s", name, text)
		}
	}
	if len(f.writes) != 0 {
		t.Fatalf("sent %v", f.writes)
	}
}

func TestKeys(t *testing.T) {
	for _, key := range []string{"sk_live_x", "rk_live_x", "pk_test_x", ""} {
		if _, err := stripemcp.New(key, token); err == nil {
			t.Errorf("key %q: accepted", key)
		}
	}
	if _, err := stripemcp.New(testKey, "short"); err == nil {
		t.Error("a short token: accepted")
	}
	// Stripe's error never comes back with the key.
	f := newStripe(t)
	s, _ := stripemcp.New("sk_test_otherotherotherother", token)
	s.Base = f.srv.URL
	srv := httptest.NewServer(s)
	defer srv.Close()
	if text, isErr := call(t, srv, "list_disputes", nil); !isErr || strings.Contains(text, "sk_test_other") {
		t.Fatalf("an error with the key: %s", text)
	}
}
