package planque_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kiff/agents/tool/planque"
)

// fake answers as planque does (contract 2): a grant view for the agent's
// own key, a 409 before the owner's phone is enrolled.
func fake(t *testing.T, enrolled bool) (*httptest.Server, *[]string) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+string(body))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Header.Get("Authorization") != "Bearer k_setup":
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"KIFF did not accept the key"}`))
		case !enrolled:
			w.WriteHeader(409)
			w.Write([]byte(`{"error":"the owner's phone is not enrolled with planque for this account yet"}`))
		case r.Method == "POST" && r.URL.Path == "/v1/accounts/tnt_1/grants":
			w.WriteHeader(201)
			w.Write([]byte(`{"id":"grt_1","status":"pending","text":"Connect your account at mcp.stripe.com…","digest":"abc","request":{"v":"planque.grant.v1"},"audience":"https://planque.example/mcp/tnt_1/grt_1","events":[{"what":"requested"}]}`))
		case r.Method == "GET" && r.URL.Path == "/v1/accounts/tnt_1/grants/grt_1":
			w.Write([]byte(`{"id":"grt_1","status":"connected","tools":["stripe_refund"],"digest":"abc"}`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"error":"no such grant"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func call(t *testing.T, tl *planque.Tool, in string) (string, bool) {
	t.Helper()
	r, err := tl.Call(context.Background(), json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	return r.Text, r.IsError
}

// A request goes to the account's grants with the agent's own key and
// exactly what it asks; the model reads the grant's id, status and the
// owner's text, nothing more.
func TestRequestAndStatus(t *testing.T) {
	srv, seen := fake(t, true)
	tl := planque.New(srv.URL+"/", "tnt_1", "k_setup", nil)
	text, isErr := call(t, tl, `{"action":"request","server":"https://mcp.stripe.com","agent":"agt_refunds","tools":[{"tool":"stripe_api_write","name":"stripe_refund"}]}`)
	if isErr || text != `{"id":"grt_1","status":"pending","text":"Connect your account at mcp.stripe.com…"}` {
		t.Fatalf("request: %s %v", text, isErr)
	}
	want := `POST /v1/accounts/tnt_1/grants Bearer k_setup {"agent":"agt_refunds","server":"https://mcp.stripe.com","tools":[{"tool":"stripe_api_write","name":"stripe_refund"}]}`
	if (*seen)[0] != want {
		t.Fatalf("sent %s\nwant %s", (*seen)[0], want)
	}
	text, isErr = call(t, tl, `{"action":"status","grant":"grt_1"}`)
	if isErr || text != `{"id":"grt_1","status":"connected","tools":["stripe_refund"]}` {
		t.Fatalf("status: %s %v", text, isErr)
	}
}

// What planque refuses, the model reads; a malformed ask never leaves.
func TestRefusalsAreReadable(t *testing.T) {
	srv, seen := fake(t, false)
	tl := planque.New(srv.URL, "tnt_1", "k_setup", nil)
	text, isErr := call(t, tl, `{"action":"request","server":"https://mcp.stripe.com","agent":"a","tools":[{"tool":"x"}]}`)
	if !isErr || !strings.Contains(text, "409") || !strings.Contains(text, "not enrolled") {
		t.Fatalf("before enrolment: %s", text)
	}
	for _, in := range []string{`{"action":"request","server":"https://x"}`, `{"action":"status"}`, `{"action":"status","grant":"../admin"}`, `{"action":"approve","grant":"grt_1"}`, `nope`} {
		if _, isErr := call(t, tl, in); !isErr {
			t.Errorf("%s was not refused", in)
		}
	}
	if len(*seen) != 1 {
		t.Fatalf("malformed asks reached planque: %v", *seen)
	}
	other := planque.New(srv.URL, "tnt_1", "k_other", nil)
	if text, isErr := call(t, other, `{"action":"status","grant":"grt_1"}`); !isErr || !strings.Contains(text, "401") {
		t.Fatalf("another key: %s", text)
	}
}
