package planque_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kiff/agents/tool/planque"
)

// A website taf is asked for on planque's browser entry with the sites the
// owner named and planque's browse tools: the tool sends exactly that, and
// refuses a browser without sites or tools, or sites on another entry.
func TestAWebsiteTafAsksPlanquesBrowserForTheNamedSites(t *testing.T) {
	var got map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.Method + " " + r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"grant":{"id":"grt_web","status":"pending"}}`))
	}))
	t.Cleanup(srv.Close)
	tl := planque.New(srv.URL, "tnt_1", "k_setup", nil)

	text, isErr := call(t, tl, `{"action":"request","entry":"browser@1","agent":"orders-a1","taf_name":"Shop orders",`+
		`"browser":{"sites":["shop.example"]},"tools":[{"tool":"browse_open","title":"Open a page"},{"tool":"browse_read","title":"Read a page"}]}`)
	if isErr || !strings.Contains(text, "grt_web") || path != "POST /v1/accounts/tnt_1/catalog/browser@1/grants" {
		t.Fatalf("request: %s (%s)", text, path)
	}
	b, _ := got["browser"].(map[string]any)
	tools, _ := got["tools"].([]any)
	if got["agent"] != "orders-a1" || got["agent_name"] != "Shop orders" || b == nil || len(b["sites"].([]any)) != 1 || b["sites"].([]any)[0] != "shop.example" || len(tools) != 2 {
		t.Fatalf("sent: %v", got)
	}

	path = ""
	for name, in := range map[string]string{
		"no sites":                 `{"action":"request","entry":"browser@1","agent":"a1","taf_name":"T","browser":{"sites":[]},"tools":[{"tool":"browse_open","title":"Open"}]}`,
		"no tools":                 `{"action":"request","entry":"browser@1","agent":"a1","taf_name":"T","browser":{"sites":["shop.example"]},"tools":[]}`,
		"no browser":               `{"action":"request","entry":"browser@1","agent":"a1","taf_name":"T","tools":[{"tool":"browse_open","title":"Open"}]}`,
		"a browser on another app": `{"action":"request","entry":"connector:composio:gmail","agent":"a1","taf_name":"T","browser":{"sites":["shop.example"]}}`,
	} {
		if text, isErr := call(t, tl, in); !isErr {
			t.Errorf("%s: %s", name, text)
		}
	}
	if path != "" {
		t.Fatalf("a refused request reached planque: %s", path)
	}
}
