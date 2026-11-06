package planque_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/planque"
)

// fake answers as planque does (contract 2): a grant view for the agent's
// own key, a 409 before the owner's phone is enrolled.
// seenMu guards what a fake saw: State reads grants at once, so its
// handlers append at the same time. A test that reads seen after concurrent
// calls holds it too.
var seenMu sync.Mutex

func fake(t *testing.T, enrolled bool) (*httptest.Server, *[]string) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenMu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+string(body))
		seenMu.Unlock()
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

// A REST vendor without an MCP server is asked for by its key: planque gets
// an api_key grant with the description, base and where the key goes, and
// no server. Never both, and never neither.
func TestARESTVendorIsAskedForByKey(t *testing.T) {
	srv, seen := fake(t, true)
	tl := planque.New(srv.URL+"/", "tnt_1", "k_setup", nil)
	api := `{"url":"https://desk.example/openapi.json","base":"https://desk.example/v1","auth":{"in":"header","name":"X-Desk-Key"}}`
	text, isErr := call(t, tl, `{"action":"request","api":`+api+`,"agent":"support-k7q3x9xz","tools":[]}`)
	if isErr || !strings.Contains(text, `"id":"grt_1"`) {
		t.Fatalf("request: %s %v", text, isErr)
	}
	want := `POST /v1/accounts/tnt_1/grants Bearer k_setup {"agent":"support-k7q3x9xz","api":` + api + `,"kind":"api_key","tools":[]}`
	if (*seen)[0] != want {
		t.Fatalf("sent %s\nwant %s", (*seen)[0], want)
	}
	for _, in := range []string{
		`{"action":"request","api":` + api + `,"server":"https://mcp.example","agent":"a","tools":[]}`,
		`{"action":"request","agent":"a","tools":[]}`,
		`{"action":"request","api":null,"agent":"a","tools":[]}`,
	} {
		if text, isErr := call(t, tl, in); !isErr || !strings.Contains(text, "not both") {
			t.Errorf("%s: %s %v", in, text, isErr)
		}
	}
	if len(*seen) != 1 {
		t.Fatalf("malformed asks reached planque: %v", *seen)
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
// each marked shortened; every tool is still listed, the note says so, and
// one tool can still be read in full.
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
		Truncated bool   `json:"truncated"`
		Note      string `json:"note"`
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
	if len(text) > planque.MaxCatalog || c.Truncated || len(c.Tools) != 60 {
		t.Fatalf("catalog of %d bytes, truncated %v, %d tools", len(text), c.Truncated, len(c.Tools))
	}
	if !strings.HasPrefix(c.Note, "Every tool is listed (60).") || !strings.Contains(c.Note, "catalog {grant, tool}") {
		t.Fatalf("note: %q", c.Note)
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

// A catalog too large even for names and short descriptions keeps every
// tool's name: the last tools lose their words, never their name, so the
// model can still ask for any of them in full (the v21 Coach check, where a
// vendor's many tools hid the one that finds a page).
func TestEveryToolKeepsItsName(t *testing.T) {
	desc := strings.Repeat("w", 300)
	var tools []string
	for i := range 1500 {
		tools = append(tools, `{"name":"vendor_tool_number_`+strconv.Itoa(i)+`","description":"`+desc+`","input_schema":{"type":"object"}}`)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"grant":"grt_1","server":"https://mcp.vendor.test","tools":[` + strings.Join(tools, ",") + `]}`))
	}))
	t.Cleanup(srv.Close)
	tl := planque.New(srv.URL, "tnt_1", "k_setup", nil)

	text, isErr := call(t, tl, `{"action":"catalog","grant":"grt_1"}`)
	var c struct {
		Truncated bool   `json:"truncated"`
		Note      string `json:"note"`
		Tools     []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Shortened   bool   `json:"shortened"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(text), &c); isErr || err != nil {
		t.Fatalf("catalog: %v %.200s", err, text)
	}
	if len(text) > planque.MaxCatalog || c.Truncated || len(c.Tools) != 1500 {
		t.Fatalf("catalog of %d bytes, truncated %v, %d of 1500 tools", len(text), c.Truncated, len(c.Tools))
	}
	if last := c.Tools[1499]; last.Name != "vendor_tool_number_1499" || last.Description != "" || !last.Shortened {
		t.Fatalf("the last tool: %+v", last)
	}
	if first := c.Tools[0]; first.Description == "" {
		t.Fatal("the first tools lost their words before the last")
	}
	if !strings.HasPrefix(c.Note, "Every tool is listed (1500).") {
		t.Fatalf("note: %q", c.Note)
	}
	text, isErr = call(t, tl, `{"action":"catalog","grant":"grt_1","tool":"vendor_tool_number_1499"}`)
	if isErr || !strings.Contains(text, desc) {
		t.Fatalf("the last tool in full: %.200s", text)
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

// gateway is KIFF's gateway as Setup's reads see it: the calls it was asked.
type gateway struct {
	name, input string
	res         tool.Result
}

func (g *gateway) Call(_ context.Context, name string, input json.RawMessage) (tool.Result, error) {
	g.name, g.input = name, string(input)
	return g.res, nil
}

// Setup reads a tool the owner approved for it (setup_reads), by the name
// the grant lists (kiff/tafs#174), through KIFF's gateway, so KIFF decides
// it on Setup's own Card and planque sends it only with KIFF's permit
// (kiff/tafs#138). Nothing goes to planque's own read.
func TestReadGoesThroughKIFF(t *testing.T) {
	var planqueCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		planqueCalls++
		http.Error(w, "gone", http.StatusGone)
	}))
	t.Cleanup(srv.Close)
	g := &gateway{res: tool.Result{Text: "fields"}}
	tl := planque.New(srv.URL, "tnt_1", "k_setup", nil).ReadThrough(g)
	if text, isErr := call(t, tl, `{"action":"read","grant":"grt_1","tool":"stripe_api_details_x1","arguments":{"stripe_api_operation_id":"GetChargesCharge"}}`); isErr ||
		text != "fields" || g.name != "stripe_api_details_x1" || g.input != `{"stripe_api_operation_id":"GetChargesCharge"}` || planqueCalls != 0 {
		t.Fatalf("%s (error %v): %s %s, planque called %d times", text, isErr, g.name, g.input, planqueCalls)
	}
	// KIFF's refusal comes back as the read's error.
	g.res = tool.Result{Text: "KIFF refused: no Card covers this", IsError: true}
	if text, isErr := call(t, tl, `{"action":"read","grant":"grt_1","tool":"stripe_api_details_x1"}`); !isErr || !strings.Contains(text, "refused") || g.input != "{}" {
		t.Fatalf("a refused read: %s %v %s", text, isErr, g.input)
	}
	if _, isErr := call(t, tl, `{"action":"read","grant":"grt_1"}`); !isErr {
		t.Fatal("a read with no tool was sent")
	}
	// No gateway to read through: nothing is read.
	if text, isErr := call(t, planque.New(srv.URL, "tnt_1", "k_setup", nil), `{"action":"read","grant":"grt_1","tool":"x"}`); !isErr || planqueCalls != 0 {
		t.Fatalf("a read with no gateway: %s %v", text, isErr)
	}
}

// describe is gone (kiff/planque#23): the action is refused before planque.
func TestThereIsNoDescribe(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = append(seen, r.URL.Path) }))
	t.Cleanup(srv.Close)
	tl := planque.New(srv.URL, "tnt_1", "k_setup", nil)
	if text, isErr := call(t, tl, `{"action":"describe","grant":"grt_1","tool":"list_accounts"}`); !isErr || !strings.Contains(text, "action is request, status, catalog, tools, read, grants, search or events") {
		t.Fatalf("describe: %s", text)
	}
	if len(seen) != 0 {
		t.Fatalf("sent to planque: %v", seen)
	}
}

// Only planque's lookups are reads: what a chat may keep of them.
func TestReadsAreTheLookups(t *testing.T) {
	tl := planque.New("https://planque.example", "tnt_1", "k_setup", nil)
	for in, want := range map[string]bool{
		`{"action":"status","grant":"g"}`: true, `{"action":"catalog","grant":"g"}`: true, `{"action":"read","grant":"g"}`: true, `{"action":"grants","agent":"a"}`: true,
		`{"action":"request","server":"s"}`: false, `{"action":"tools","grant":"g"}`: false, `not json`: false,
	} {
		if got := tl.Reads(json.RawMessage(in)); got != want {
			t.Errorf("Reads(%s) = %v, want %v", in, got, want)
		}
	}
}

// State is each grant the chat's components name, as planque says now,
// read with the agent's own key; with none named, planque isn't asked.
func TestStateSaysWhereEachShownGrantStands(t *testing.T) {
	srv, seen := fake(t, true)
	tl := planque.New(srv.URL, "tnt_1", "k_setup", nil)
	shown := []json.RawMessage{
		json.RawMessage(`{"type":"approval","id":"a1","grant":"grt_1"}`),
		json.RawMessage(`{"type":"plan","steps":[]}`),
		json.RawMessage(`{"type":"approval","id":"a2","grant":"grt_9"}`),
		json.RawMessage(`{"type":"approval","id":"a3","grant":"grt_1"}`),
	}
	text, err := tl.State(context.Background(), shown)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, `"id":"grt_1","status":"connected","tools":["stripe_refund"]`) || !strings.Contains(text, "grt_9: planque answered 404: no such grant") {
		t.Fatalf("State said %q", text)
	}
	if strings.Index(text, "grt_9") > strings.Index(text, "grt_1") {
		t.Fatalf("the grants aren't in the order last shown: %q", text)
	}
	seenMu.Lock()
	asked := slices.Clone(*seen)
	*seen = nil
	seenMu.Unlock()
	// The grants are read at once, so in any order: sorted, the two distinct grants, each once.
	slices.Sort(asked)
	if len(asked) != 2 || !strings.Contains(asked[0], "/grants/grt_1 ") || !strings.Contains(asked[1], "/grants/grt_9 ") {
		t.Fatalf("State asked %v", asked)
	}
	for _, s := range asked {
		if !strings.HasPrefix(s, "GET ") || !strings.Contains(s, "Bearer k_setup") {
			t.Fatalf("State made %q", s)
		}
	}
	text, _ = tl.State(context.Background(), shown[1:2])
	seenMu.Lock()
	n := len(*seen)
	seenMu.Unlock()
	if text != "" || n != 0 {
		t.Fatalf("with no grant shown, State said %q and asked planque %d times", text, n)
	}
}

// Improving a taf, Setup finds the connections it asked for the taf's agent
// and adds to the one it has for a vendor (kiff/tafs#138): grants {agent}
// asks planque with Setup's own key, and tools on a connected grant adds.
func TestGrantsListsATafsConnections(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"grants":[{"id":"grt_1","vendor":"Stripe","server":"https://mcp.stripe.com","status":"connected","tools":[{"name":"create_refund_gtqe","tool":"create_refund"}]}]}`))
	}))
	t.Cleanup(srv.Close)
	tl := planque.New(srv.URL, "tnt_1", "k_setup", nil)
	text, isErr := call(t, tl, `{"action":"grants","agent":"taf-refunds-x7k3"}`)
	if isErr || !strings.Contains(text, `"id":"grt_1"`) || !strings.Contains(text, `"server":"https://mcp.stripe.com"`) {
		t.Fatalf("grants: %s", text)
	}
	if len(seen) != 1 || seen[0] != "GET /v1/accounts/tnt_1/agents/taf-refunds-x7k3/grants Bearer k_setup" {
		t.Fatalf("asked: %v", seen)
	}
	if _, isErr := call(t, tl, `{"action":"grants","agent":"../x"}`); !isErr || len(seen) != 1 {
		t.Errorf("a path in the agent should be refused before planque: %v", seen)
	}
	// With no agent, every connection Setup asked for: a lost id is found again (kiff/planque#49).
	if text, isErr := call(t, tl, `{"action":"grants"}`); isErr || !strings.Contains(text, `"id":"grt_1"`) {
		t.Fatalf("grants with no agent: %s", text)
	}
	if len(seen) != 2 || seen[1] != "GET /v1/accounts/tnt_1/asked-grants Bearer k_setup" {
		t.Fatalf("asked with no agent: %v", seen)
	}
	if spec := tl.Spec(); !strings.Contains(spec.Description, "On a connected grant, tools {grant, tools} adds the tools sent") ||
		!strings.Contains(spec.Description, "one connection per vendor") || !strings.Contains(spec.Description, "grants with no agent lists every connection") {
		t.Fatalf("the description doesn't say how to add: %s", spec.Description)
	}
}

// Before connecting anything, search finds tools across apps by words, with
// the agent's key, and events lists an app's events by its catalog entry,
// then by its grant once connected (kiff/planque#87). A malformed ask never
// leaves the tool.
func TestSearchAndEventsBeforeConnecting(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/accounts/tnt_1/catalog/tools":
			w.Write([]byte(`{"tools":[{"entry":"connector:composio:quickbooks","app":"quickbooks","app_title":"QuickBooks","app_state":"available","slug":"QUICKBOOKS_LIST_INVOICES","name":"List invoices","read_only":true},{"entry":"connector:composio:own","app":"own","app_title":"Own app","app_state":"needs_setup","slug":"OWN_SEND","name":"Send"}],"provider":"composio","next_cursor":"p2","skipped":1}`))
		case "/v1/accounts/tnt_1/catalog/connector:composio:quickbooks/grants":
			b, _ := io.ReadAll(r.Body)
			seen[len(seen)-1] += " " + string(b)
			// As planque answers a catalog grant: wrapped, with needs_key.
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"grant":{"id":"grt_9","status":"pending","text":"Connect QuickBooks for Invoices…","on_demand":true,"custody":"provider","request":{}},"needs_key":false}`))
		case "/v1/accounts/tnt_1/catalog/connector:composio:quickbooks/events", "/v1/accounts/tnt_1/grants/grt_1/events":
			w.Write([]byte(`{"events":[{"slug":"QUICKBOOKS_NEW_INVOICE","name":"New invoice","version":"20261101_00","config":{"type":"object"},"door":false}],"next_cursor":""}`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"error":"no such route"}`))
		}
	}))
	t.Cleanup(srv.Close)
	tl := planque.New(srv.URL, "tnt_1", "k_setup", nil)
	text, isErr := call(t, tl, `{"action":"search","q":" overdue invoice ","apps":["quickbooks","own"]}`)
	if isErr || !strings.Contains(text, `"app_state":"needs_setup"`) || !strings.Contains(text, `"QUICKBOOKS_LIST_INVOICES"`) || !strings.Contains(text, `"next_cursor":"p2"`) {
		t.Fatalf("search: %s", text)
	}
	if len(seen) != 1 || seen[0] != "GET /v1/accounts/tnt_1/catalog/tools?apps=quickbooks%2Cown&q=overdue+invoice Bearer k_setup" {
		t.Fatalf("asked: %v", seen)
	}
	if text, isErr := call(t, tl, `{"action":"search","q":"invoice","cursor":"p2"}`); isErr || !strings.HasSuffix(seen[len(seen)-1], "/catalog/tools?cursor=p2&q=invoice Bearer k_setup") {
		t.Fatalf("a search continued: %s %v", text, seen)
	}
	for _, bad := range []string{`{"action":"search"}`, `{"action":"search","q":"   "}`, `{"action":"search","q":"x","apps":["a/b"]}`, `{"action":"search","q":"x","apps":["a b"]}`} {
		if _, isErr := call(t, tl, bad); !isErr {
			t.Errorf("not refused: %s", bad)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("a malformed search reached planque: %v", seen)
	}
	// Events by the catalog entry, before any connection, then by the grant.
	text, isErr = call(t, tl, `{"action":"events","entry":"connector:composio:quickbooks"}`)
	if isErr || !strings.Contains(text, `"QUICKBOOKS_NEW_INVOICE"`) || seen[len(seen)-1] != "GET /v1/accounts/tnt_1/catalog/connector:composio:quickbooks/events Bearer k_setup" {
		t.Fatalf("events by entry: %s %v", text, seen)
	}
	if text, isErr := call(t, tl, `{"action":"events","grant":"grt_1","cursor":"p2"}`); isErr || seen[len(seen)-1] != "GET /v1/accounts/tnt_1/grants/grt_1/events?cursor=p2 Bearer k_setup" {
		t.Fatalf("events by grant: %s %v", text, seen)
	}
	for _, bad := range []string{`{"action":"events"}`, `{"action":"events","entry":"a","grant":"b"}`, `{"action":"events","entry":"../x"}`, `{"action":"events","grant":"g?x"}`} {
		if _, isErr := call(t, tl, bad); !isErr {
			t.Errorf("not refused: %s", bad)
		}
	}
	if len(seen) != 4 {
		t.Fatalf("a malformed events ask reached planque: %v", seen)
	}
	// The chosen app is connected by its entry: no URL, no tools yet, the
	// taf's agent named; planque answers the grant to approve.
	text, isErr = call(t, tl, `{"action":"request","entry":"connector:composio:quickbooks","name":"Invoices"}`)
	var made struct{ ID, Agent string }
	_ = json.Unmarshal([]byte(text), &made)
	if isErr || made.ID != "grt_9" || !strings.HasPrefix(made.Agent, "invoices-") ||
		seen[len(seen)-1] != "POST /v1/accounts/tnt_1/catalog/connector:composio:quickbooks/grants Bearer k_setup "+`{"agent":"`+made.Agent+`"}` {
		t.Fatalf("request by entry: %s %v", text, seen)
	}
	for _, bad := range []string{
		`{"action":"request","entry":"connector:composio:quickbooks"}`,
		`{"action":"request","entry":"connector:composio:quickbooks","server":"https://x.example","agent":"a"}`,
		`{"action":"request","entry":"../grants","agent":"a"}`,
		`{"action":"request","entry":"connector:composio:quickbooks","agent":"a","tools":[{"tool":"QUICKBOOKS_LIST_INVOICES"}]}`,
	} {
		if _, isErr := call(t, tl, bad); !isErr {
			t.Errorf("not refused: %s", bad)
		}
	}
	if len(seen) != 5 {
		t.Fatalf("a malformed request reached planque: %v", seen)
	}
	// A reviewed entry may answer the grant flat: read as well.
	flat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"grt_8","status":"pending","text":"Connect Desk for Invoices…","request":{}}`))
	}))
	t.Cleanup(flat.Close)
	if text, isErr := call(t, planque.New(flat.URL, "tnt_1", "k_setup", nil), `{"action":"request","entry":"desk","agent":"a"}`); isErr || !strings.Contains(text, `"id":"grt_8"`) || !strings.Contains(text, `"agent":"a"`) {
		t.Fatalf("a flat grant answer: %s", text)
	}
	// Both only read; the model is told when to use them.
	if !tl.Reads(json.RawMessage(`{"action":"search","q":"x"}`)) || !tl.Reads(json.RawMessage(`{"action":"events","grant":"g"}`)) {
		t.Fatal("search and events read")
	}
	// A grant set up on demand says so to the model, with who holds the sign-in.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"grt_2","status":"pending","text":"Connect AgentMail for Invoices…","on_demand":true,"custody":"provider","digest":"x","request":{}}`))
	}))
	t.Cleanup(srv2.Close)
	if text, isErr := call(t, planque.New(srv2.URL, "tnt_1", "k_setup", nil), `{"action":"status","grant":"grt_2"}`); isErr || !strings.Contains(text, `"on_demand":true`) || !strings.Contains(text, `"custody":"provider"`) || strings.Contains(text, `"digest"`) {
		t.Fatalf("an on-demand grant: %s", text)
	}
	if spec := tl.Spec(); !strings.Contains(spec.Description, "search {q} finds tools across every app") || !strings.Contains(spec.Description, "events {entry} lists an app's events") || !strings.Contains(spec.Description, "on_demand: true") || !strings.Contains(spec.Description, "request {entry}") || !strings.Contains(string(spec.InputSchema), `"search","events"`) {
		t.Fatalf("the description doesn't say how to search: %s", spec.Description)
	}
}

// A long catalog is searched by words rather than given up on: every page
// is read, the tools whose name or description has every word come back in
// full, and no match says so without claiming the tool is absent.
func TestCatalogSearchesByWords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			w.Write([]byte(`{"grant":"grt_1","server":"https://mcp.notion.example","account":"acc_1","tools":[` +
				`{"name":"create_page","description":"Creates a page in a database.","input_schema":{"type":"object","properties":{"title":{"type":"string"}}}},` +
				`{"name":"list_users","description":"Lists the workspace's users."}],"next_cursor":"p2"}`))
			return
		}
		w.Write([]byte(`{"grant":"grt_1","server":"https://mcp.notion.example","account":"acc_1","tools":[` +
			`{"name":"search","description":"Finds a page or database by its title.","input_schema":{"type":"object","properties":{"query":{"type":"string"}}},"read_only":true},` +
			`{"name":"retrieve_page","description":"Reads one page by its id."}]}`))
	}))
	t.Cleanup(srv.Close)
	tl := planque.New(srv.URL, "tnt_1", "k_setup", nil)
	text, isErr := call(t, tl, `{"action":"catalog","grant":"grt_1","q":"Find page"}`)
	if isErr || !strings.Contains(text, `"name":"search"`) || strings.Contains(text, `"create_page"`) || strings.Contains(text, `"list_users"`) || !strings.Contains(text, `"query"`) {
		t.Fatalf("by words: %s", text)
	}
	if !strings.Contains(text, `1 tool matches \"Find page\", in full.`) {
		t.Fatalf("the note: %s", text)
	}
	// One word matches more, across both pages, in the vendor's order.
	text, _ = call(t, tl, `{"action":"catalog","grant":"grt_1","q":"page"}`)
	var got struct {
		Tools []struct{ Name string } `json:"tools"`
	}
	_ = json.Unmarshal([]byte(text), &got)
	names := []string{}
	for _, tl := range got.Tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, " ") != "create_page search retrieve_page" {
		t.Fatalf("matches: %v", names)
	}
	// No match says so, and never that the catalog was read when it wasn't.
	if text, isErr := call(t, tl, `{"action":"catalog","grant":"grt_1","q":"refund"}`); isErr || !strings.Contains(text, "No tool's name or description has every word") {
		t.Fatalf("no match: %s", text)
	}
	if _, isErr := call(t, tl, `{"action":"catalog","grant":"grt_1","q":"page","tool":"search"}`); !isErr {
		t.Fatal("tool and q together should be refused")
	}
	if spec := tl.Spec(); !strings.Contains(spec.Description, "A long catalog is never a reason to stop: catalog {grant, q}") {
		t.Fatalf("the description doesn't say to search: %s", spec.Description)
	}
}

// A dot segment as a grant, entry or agent never leaves the tool: it would
// go into planque's path as "." or "..", not as an id.
func TestADotSegmentIsNeverSent(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = append(seen, r.URL.Path) }))
	t.Cleanup(srv.Close)
	tl := planque.New(srv.URL, "tnt_1", "k_setup", nil)
	for _, in := range []string{
		`{"action":"status","grant":"."}`, `{"action":"catalog","grant":".."}`, `{"action":"events","grant":"."}`,
		`{"action":"events","entry":".."}`, `{"action":"grants","agent":"."}`, `{"action":"tools","grant":"..","tools":[{"tool":"x"}]}`,
	} {
		if _, isErr := call(t, tl, in); !isErr {
			t.Errorf("not refused: %s", in)
		}
	}
	if len(seen) != 0 {
		t.Fatalf("sent to planque: %v", seen)
	}
}

// A managed app's tools go with the profile Coach drafted, as planque
// takes them; without one, the tools alone, as before. A profile that isn't
// an object never reaches planque.
func TestToolsCarryAManagedAppsProfile(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, r.Method+" "+r.URL.Path+" "+string(b))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"grt_9","status":"pending","text":"Connect Agent Mail…","request":{}}`))
	}))
	t.Cleanup(srv.Close)
	tl := planque.New(srv.URL, "tnt_1", "k_setup", nil)
	profile := `{"v":"kiff.profile.v1","vendor":"Agent Mail","transport":"rest","pin":{"kind":"none"},"source":"drafted","test_proven":false,"operations":[{"tool":"send_reminder","title":"Send a reminder email","kind":"write","effect":"sends","shape":"single"}]}`
	tools := `[{"tool":"connector_8cd3","name":"send_reminder","title":"Send a reminder email"}]`
	if text, isErr := call(t, tl, `{"action":"tools","grant":"grt_9","tools":`+tools+`,"profile":`+profile+`}`); isErr {
		t.Fatalf("tools with a profile: %s", text)
	}
	var sent struct {
		Tools   json.RawMessage `json:"tools"`
		Profile json.RawMessage `json:"profile"`
	}
	if len(bodies) != 1 || !strings.HasPrefix(bodies[0], "POST /v1/accounts/tnt_1/grants/grt_9/tools ") ||
		json.Unmarshal([]byte(strings.SplitN(bodies[0], " ", 3)[2]), &sent) != nil || string(sent.Profile) != profile || string(sent.Tools) != tools {
		t.Fatalf("sent: %v", bodies)
	}
	if _, isErr := call(t, tl, `{"action":"tools","grant":"grt_9","tools":`+tools+`}`); isErr || strings.Contains(bodies[1], `"profile"`) {
		t.Fatalf("tools alone: %v", bodies)
	}
	if _, isErr := call(t, tl, `{"action":"tools","grant":"grt_9","tools":`+tools+`,"profile":"a profile"}`); !isErr || len(bodies) != 2 {
		t.Fatalf("a profile that isn't an object reached planque: %v", bodies)
	}
	if spec := tl.Spec(); !strings.Contains(spec.Description, "tools {grant, tools, profile}") || !strings.Contains(string(spec.InputSchema), `"profile"`) {
		t.Fatal("the words don't say how to send a managed app's tools")
	}
}

// The description says how each grant reaches its vendor, and to read the
// account's connections before proposing an app for a vendor it already
// connects, so a new taf is offered the same kind first.
func TestTheDescriptionSaysEachConnectionsKind(t *testing.T) {
	d := planque.New("http://planque.invalid", "tnt_1", "k_setup", nil).Spec().Description
	for _, want := range []string{"vendor_sign_in", "provider", "key (a REST key", "mode_words",
		"read grants with no agent: the same kind is the one to offer first"} {
		if !strings.Contains(d, want) {
			t.Fatalf("the description doesn't say %q: %s", want, d)
		}
	}
}

// A run's start waits for the slowest grant read, not their sum: each grant on
// screen is read at once, and the lines keep the grants' order.
func TestStateReadsTheGrantsAtOnce(t *testing.T) {
	const delay = 150 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		w.Write([]byte(`{"id":"` + id + `","status":"connected"}`))
	}))
	t.Cleanup(srv.Close)
	var shown []json.RawMessage
	for i := range planque.MaxStated {
		shown = append(shown, json.RawMessage(`{"type":"approval","grant":"grt_`+strconv.Itoa(i)+`"}`))
	}
	began := time.Now()
	text, err := planque.New(srv.URL, "tnt_1", "k_setup", nil).State(context.Background(), shown)
	took := time.Since(began)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d grants at %v each: %v (one after another: %v)", planque.MaxStated, delay, took.Round(time.Millisecond), delay*planque.MaxStated)
	if took > 3*delay {
		t.Fatalf("%d grants took %v, one after another", planque.MaxStated, took)
	}
	for i := range planque.MaxStated {
		at := strings.Index(text, `"grt_`+strconv.Itoa(i)+`"`)
		if at < 0 || (i > 0 && at < strings.Index(text, `"grt_`+strconv.Itoa(i-1)+`"`)) {
			t.Fatalf("grant %d missing or out of order: %s", i, text)
		}
	}
}

// The taf's name goes to planque as agent_name, on one line, for the words
// the owner reads ("Connect Stripe for Overdue invoice reminders"), on every
// kind of request; without one, the request is as before (kiff/tafs#476).
func TestARequestNamesTheTaf(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"grant":{"id":"grt_9","status":"pending","text":"Connect…","request":{}},"id":"grt_9","status":"pending","text":"Connect…","request":{}}`))
	}))
	t.Cleanup(srv.Close)
	tl := planque.New(srv.URL, "tnt_1", "k_setup", nil)
	for _, in := range []string{
		`{"action":"request","server":"https://mcp.stripe.com","name":"reminders","taf_name":" Overdue invoice\nreminders "}`,
		`{"action":"request","entry":"connector:composio:agentmail","agent":"reminders-5mgnsmce","taf_name":"Overdue invoice reminders"}`,
		`{"action":"request","api":{"url":"https://x.example/openapi.json","base":"https://x.example","auth":{"in":"header","name":"X-Key"}},"name":"reminders","taf_name":"Overdue invoice reminders"}`,
	} {
		if text, isErr := call(t, tl, in); isErr {
			t.Fatalf("%s: %s", in, text)
		}
	}
	if len(bodies) != 3 {
		t.Fatalf("sent %d requests", len(bodies))
	}
	for _, b := range bodies {
		if b["agent_name"] != "Overdue invoice reminders" {
			t.Fatalf("sent %v", b)
		}
	}
	if _, isErr := call(t, tl, `{"action":"request","server":"https://mcp.stripe.com","name":"reminders"}`); isErr {
		t.Fatal("a request without a name")
	}
	if _, ok := bodies[3]["agent_name"]; ok {
		t.Fatalf("a request without a name sent one: %v", bodies[3])
	}
}
