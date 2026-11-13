package planque_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kiff/agents/tool/planque"
)

// An app connected first by its entry takes Setup's reads in its first
// request, approved with the sign-in; a server's request can't, and a
// malformed list never leaves the tool. Tools the owner declined are
// withdrawn from KIFF by their grant.
func TestSetupReadsWithTheSignInAndAWithdrawal(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.Path+" "+string(b))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/accounts/tnt_1/catalog/stripe@1/grants":
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"grant":{"id":"grt_5","status":"pending","text":"Connect Stripe…"},"needs_key":false}`))
		case r.URL.Path == "/v1/accounts/tnt_1/grants/grt_5/tools" && r.Method == http.MethodDelete:
			w.Write([]byte(`{"id":"grt_5","status":"connected","tools":["stripe_api_details_x"]}`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"error":"no such route"}`))
		}
	}))
	t.Cleanup(srv.Close)
	tl := planque.New(srv.URL, "tnt_1", "k_setup", nil)

	text, isErr := call(t, tl, `{"action":"request","entry":"stripe@1","agent":"refunds-a1","setup_reads":["stripe_api_details","list_accounts"]}`)
	if isErr || seen[len(seen)-1] != `POST /v1/accounts/tnt_1/catalog/stripe@1/grants {"agent":"refunds-a1","setup_reads":["stripe_api_details","list_accounts"]}` {
		t.Fatalf("request with setup reads: %s %v", text, seen)
	}
	for _, bad := range []string{
		`{"action":"request","server":"https://mcp.example","agent":"refunds-a1","setup_reads":["stripe_api_details"]}`,
		`{"action":"request","entry":"stripe@1","agent":"refunds-a1","setup_reads":["a","a"]}`,
		`{"action":"request","entry":"stripe@1","agent":"refunds-a1","setup_reads":[" a"]}`,
		`{"action":"request","entry":"stripe@1","agent":"refunds-a1","setup_reads":["a","b","c","d","e","f","g","h","i","j","k"]}`,
	} {
		if text, isErr := call(t, tl, bad); !isErr {
			t.Errorf("not refused: %s → %s", bad, text)
		}
	}
	if len(seen) != 1 {
		t.Fatalf("a refused ask reached planque: %v", seen)
	}
	if text, _ := call(t, tl, `{"action":"request","server":"https://mcp.example","agent":"refunds-a1","setup_reads":["x"]}`); !strings.Contains(text, "setup_reads goes with request {entry}") {
		t.Fatalf("a server's setup_reads: %s", text)
	}

	text, isErr = call(t, tl, `{"action":"withdraw","grant":"grt_5"}`)
	if isErr || !strings.Contains(text, `"status":"connected"`) || seen[len(seen)-1] != "DELETE /v1/accounts/tnt_1/grants/grt_5/tools " {
		t.Fatalf("withdraw: %s %v", text, seen)
	}
	if _, isErr := call(t, tl, `{"action":"withdraw"}`); !isErr {
		t.Fatal("a withdrawal with no grant wasn't refused")
	}
}
