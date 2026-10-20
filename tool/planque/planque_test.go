package planque_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
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
	text, isErr := call(t, tl, `{"action":"request","server":"https://mcp.stripe.com","name":"Refunds","tools":[{"tool":"stripe_api_write","name":"stripe_refund"}]}`)
	var g struct{ Agent string }
	json.Unmarshal([]byte(text), &g)
	if isErr || !regexp.MustCompile(`^refunds-[a-z2-7]{8}$`).MatchString(g.Agent) ||
		text != `{"id":"grt_1","agent":"`+g.Agent+`","status":"pending","text":"Connect your account at mcp.stripe.com…"}` {
		t.Fatalf("request: %s %v", text, isErr)
	}
	// planque is asked for the agent the model reads back, random part and all.
	want := `POST /v1/accounts/tnt_1/grants Bearer k_setup {"agent":"` + g.Agent + `","server":"https://mcp.stripe.com","tools":[{"tool":"stripe_api_write","name":"stripe_refund"}]}`
	if (*seen)[0] != want {
		t.Fatalf("sent %s\nwant %s", (*seen)[0], want)
	}
	// An agent id already made (a Setup on older instructions) is sent as given.
	text, isErr = call(t, tl, `{"action":"request","server":"https://mcp.stripe.com","agent":"reembolsos-k7q3x9","tools":[]}`)
	if isErr || !strings.Contains(text, `"agent":"reembolsos-k7q3x9"`) || !strings.Contains((*seen)[len(*seen)-1], `"agent":"reembolsos-k7q3x9"`) {
		t.Fatalf("request with an agent id: %s %v, sent %s", text, isErr, (*seen)[len(*seen)-1])
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
	for _, in := range []string{`{"action":"request","server":"https://x"}`, `{"action":"request","server":"https://x","name":"r","agent":"r-1"}`, `{"action":"status"}`, `{"action":"status","grant":"../admin"}`, `{"action":"approve","grant":"grt_1"}`, `nope`} {
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

// Connect first (contract 5): a request with no tools asks for [], the
// catalog comes back trimmed to the vendor's own words and arguments, and
// the tools are set on the same grant.
func TestConnectFirstThenCatalogThenTools(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.Path+" "+string(body))
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/accounts/tnt_1/grants":
			w.WriteHeader(201)
			w.Write([]byte(`{"id":"grt_1","status":"pending","text":"Connect your account at mcp.stripe.com so agt_setup can see what it offers…","digest":"d1"}`))
		case "GET /v1/accounts/tnt_1/grants/grt_1/catalog":
			w.Write([]byte(`{"grant":"grt_1","server":"https://mcp.stripe.com","account":"acct_1","tools":[{"name":"stripe_api_write","title":"Write","description":"Any write.","input_schema":{"type":"object"},"read_only":false},{"name":"stripe_api_read","read_only":true}]}`))
		case "POST /v1/accounts/tnt_1/grants/grt_1/tools":
			w.Write([]byte(`{"id":"grt_1","status":"pending","text":"… • stripe_refund …","account":"acct_1","digest":"d2"}`))
		}
	}))
	t.Cleanup(srv.Close)
	tl := planque.New(srv.URL, "tnt_1", "k_setup", nil)

	if text, isErr := call(t, tl, `{"action":"request","server":"https://mcp.stripe.com","agent":"agt_refunds"}`); isErr || !strings.Contains(text, `"status":"pending"`) {
		t.Fatalf("connect first: %s", text)
	}
	if !strings.HasSuffix(seen[0], `"tools":[]}`) {
		t.Fatalf("a request with no tools asked for: %s", seen[0])
	}
	text, isErr := call(t, tl, `{"action":"catalog","grant":"grt_1"}`)
	if isErr || !strings.Contains(text, `"account":"acct_1"`) || !strings.Contains(text, `"input_schema":{"type":"object"}`) || strings.Contains(text, `"title"`) {
		t.Fatalf("catalog: %s", text)
	}
	text, isErr = call(t, tl, `{"action":"tools","grant":"grt_1","tools":[{"tool":"stripe_api_write","name":"stripe_refund"}]}`)
	if isErr || !strings.Contains(text, `"account":"acct_1"`) || strings.Contains(text, "digest") {
		t.Fatalf("tools: %s", text)
	}
	if seen[2] != `POST /v1/accounts/tnt_1/grants/grt_1/tools {"tools":[{"tool":"stripe_api_write","name":"stripe_refund"}]}` {
		t.Fatalf("tools sent: %s", seen[2])
	}
	for _, in := range []string{`{"action":"tools","grant":"grt_1"}`, `{"action":"tools","grant":"grt_1","tools":[]}`, `{"action":"catalog","grant":"../x"}`} {
		if _, isErr := call(t, tl, in); !isErr {
			t.Errorf("%s should be refused before planque", in)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("refused inputs reached planque: %v", seen[3:])
	}
}

// A vendor's catalog is bounded before the model reads it: long
// descriptions are cut, and the arguments that don't fit are left out,
// each marked shortened; one tool can still be read in full.
func TestALargeCatalogIsBounded(t *testing.T) {
	long := strings.Repeat("d", 5000)
	schema := `{"type":"object","properties":{"p":{"type":"string","description":"` + strings.Repeat("s", 4000) + `"}}}`
	var tools []string
	for i := range 60 {
		tools = append(tools, `{"name":"tool_`+strconv.Itoa(i)+`","description":"`+long+`","input_schema":`+schema+`}`)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"grant":"grt_1","server":"https://mcp.vendor.test","tools":[` + strings.Join(tools, ",") + `]}`))
	}))
	t.Cleanup(srv.Close)
	tl := planque.New(srv.URL, "tnt_1", "k_setup", nil)

	text, isErr := call(t, tl, `{"action":"catalog","grant":"grt_1"}`)
	var c struct {
		Truncated bool `json:"truncated"`
		Tools     []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
			Shortened   bool            `json:"shortened"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(text), &c); isErr || err != nil {
		t.Fatalf("catalog: %v %.200s", err, text)
	}
	if len(text) > planque.MaxCatalog || !c.Truncated || len(c.Tools) != 60 {
		t.Fatalf("catalog of %d bytes, truncated %v, %d tools", len(text), c.Truncated, len(c.Tools))
	}
	if n := len([]rune(c.Tools[0].Description)); n > planque.MaxDescription+1 || !c.Tools[0].Shortened {
		t.Fatalf("a description of %d characters", n)
	}
	if last := c.Tools[59]; last.InputSchema != nil || !last.Shortened {
		t.Fatalf("the last tool kept its arguments in a full catalog: %v", last.Shortened)
	}

	text, isErr = call(t, tl, `{"action":"catalog","grant":"grt_1","tool":"tool_59"}`)
	if isErr || !strings.Contains(text, `"name":"tool_59"`) || !strings.Contains(text, long) || strings.Contains(text, "tool_58") {
		t.Fatalf("one tool in full: %.200s", text)
	}
	if _, isErr := call(t, tl, `{"action":"catalog","grant":"grt_1","tool":"tool_x"}`); !isErr {
		t.Fatal("a tool the catalog doesn't have")
	}
}

// The random part of a taf's agent id comes from the tool, not the model,
// which repeats its "random" choices (kiff/tafs#78): the same name twice is
// two agents. The name is made an id, and the whole is at most 40.
func TestAgentIDIsNewEachTime(t *testing.T) {
	a, b := planque.AgentID("reembolsos"), planque.AgentID("reembolsos")
	if a == b || !strings.HasPrefix(a, "reembolsos-") {
		t.Fatalf("%s and %s", a, b)
	}
	id := regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	for name, prefix := range map[string]string{
		"Reembolsos de Tienda":  "reembolsos-de-tienda-",
		"agt_refunds":           "agt-refunds-",
		"  ¡Pagos!  ":           "pagos-",
		"Facturación Ñandú":     "facturacion-nandu-",
		"":                      "taf-",
		strings.Repeat("a", 60): strings.Repeat("a", 31) + "-",
	} {
		got := planque.AgentID(name)
		if !strings.HasPrefix(got, prefix) || len(got) > 40 || !id.MatchString(got) {
			t.Errorf("%q: %q", name, got)
		}
	}
}

// describe is gone (kiff/planque#23): the action is refused before planque.
func TestThereIsNoDescribe(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = append(seen, r.URL.Path) }))
	t.Cleanup(srv.Close)
	tl := planque.New(srv.URL, "tnt_1", "k_setup", nil)
	if text, isErr := call(t, tl, `{"action":"describe","grant":"grt_1","tool":"list_accounts"}`); !isErr || !strings.Contains(text, "action is request, status, catalog or tools") {
		t.Fatalf("describe: %s", text)
	}
	if len(seen) != 0 {
		t.Fatalf("sent to planque: %v", seen)
	}
}
