// Package planque is the tool through which an agent asks planque
// (kiff/planque) for access to a vendor account, and follows the request:
// planque keeps the vendor's sign-in, and the owner approves on their phone
// (kiff/agents#91, contract 2 on kiff/planque#1).
//
// Asking grants nothing: a grant is pending until the owner's phone signs
// it, and connected only after the owner signs in at the vendor and the
// phone confirms. So the tool is an Own tool, like memory. It calls planque
// with the agent's own KIFF key, which planque checks with KIFF; it never
// sees a vendor token.
package planque

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/tool"
)

// Name is the tool's name, as the model sees it.
const Name = "planque"

// Tool is one agent's planque.
type Tool struct {
	base, tenant, key string
	http              *http.Client
}

// New returns the planque tool for the agent whose KIFF key is key, on the
// planque at base (for example https://planque.kiff.dev).
func New(base, tenant, key string, hc *http.Client) *Tool {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Tool{base: strings.TrimRight(base, "/"), tenant: tenant, key: key, http: hc}
}

func (t *Tool) Access() tool.Access { return tool.Own }
func (t *Tool) Gateway() bool       { return false }

func (t *Tool) Spec() model.ToolSpec {
	return model.ToolSpec{
		Name: Name,
		Description: "Ask planque for access to a vendor account for a taf, and follow the request. planque keeps the vendor's sign-in; you never see a token. " +
			"To look before you choose: request {server, agent} with no tools connects first. Once the owner approved and signed in, the grant is listed, " +
			"and catalog {grant} shows the vendor's tools (name, description, arguments, read_only) and the account the sign-in acts in; nothing is called. " +
			"A large catalog is shortened (truncated: true): catalog {grant, tool} shows one tool in full. " +
			"Then tools {grant, tools} sets the taf's tools from that catalog, and the owner approves them, with no second sign-in. " +
			"request {server, agent, tools} asks for known tools at once. server is the vendor's MCP server URL (e.g. https://mcp.stripe.com), " +
			"name a short name for a new taf's KIFF agent, from the taf's name (e.g. refunds): the tool makes the agent's id from it, with a random part so it is new, " +
			"and the answer's agent is that id, to use from then on (its draft, its result); agent, instead of name, is an id already made, sent as given. Each tool is {tool, name?, read_only?, amount_argument?, amount_unit?, amount_scale?, resource_argument?, unit_lookup?, fixed_arguments?, arguments?}: " +
			"fixed_arguments pins a vendor argument (by dotted path) to a value; arguments are then the only ones the taf may give, each {path, type, required?}. " +
			"resource_argument names the string argument the call acts on (the payment, charge or order), one the taf's read of it takes. " +
			"unit_lookup {tool, argument, path} names, for a tool that moves money, the read of the same request that says the currency: " +
			"the read's name (not its vendor tool), its argument that takes that id, and where the currency is in its answer; KIFF refuses a call in another currency than the Card's. " +
			`e.g. {"tool":"create_refund","amount_argument":"amount","amount_unit":"EUR","amount_scale":2,"resource_argument":"charge",` +
			`"unit_lookup":{"tool":"get_charge","argument":"charge","path":"currency"},` +
			`"arguments":{"charge":{"path":"charge","type":"string","required":true},"amount":{"path":"amount","type":"integer","required":true}}} ` +
			"makes the amount and the charge required, so no refund leaves them out. " +
			"Each answer is the grant's id, status and the text the owner will read. Asking grants nothing: show an approval with the grant id. " +
			"status {grant}: pending, approved, signed_in, listed (signed in, tools listed, none given yet), connected (the tools are in KIFF), " +
			"failed (with a reason) or disconnected. Only connected means the taf can use the tools; the owner's word in the chat is not enough.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"action":{"type":"string","enum":["request","status","catalog","tools"]},` +
			`"server":{"type":"string","description":"the vendor's MCP server URL (request)"},` +
			`"name":{"type":"string","description":"a short name for a new taf's KIFF agent, from the taf's name (request); the answer's agent is its id"},` +
			`"agent":{"type":"string","description":"the taf's KIFF agent id, already made (request); not with name"},` +
			`"tools":{"type":"array","description":"the vendor tools to grant (request, tools); none to connect first","items":{"type":"object"}},` +
			`"grant":{"type":"string","description":"the grant's id (status, catalog, tools)"},` +
			`"tool":{"type":"string","description":"one tool of the catalog, to read it in full (catalog)"}},` +
			`"required":["action"]}`),
	}
}

// Call asks for a grant or reads one. What planque refuses comes back as a
// result the model can read and correct.
func (t *Tool) Call(ctx context.Context, input json.RawMessage) (tool.Result, error) {
	var in struct {
		Action string          `json:"action"`
		Server string          `json:"server"`
		Agent  string          `json:"agent"`
		Name   string          `json:"name"`
		Tools  json.RawMessage `json:"tools"`
		Grant  string          `json:"grant"`
		Tool   string          `json:"tool"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return tool.Result{Text: "the input must be {action, …}", IsError: true}, nil
	}
	path := "/v1/accounts/" + url.PathEscape(t.tenant) + "/grants"
	switch in.Action {
	case "request":
		if in.Server == "" || (in.Agent == "") == (in.Name == "") {
			return tool.Result{Text: "request needs server, and either name (a new taf's agent) or agent (an id already made); and tools, unless you connect first", IsError: true}, nil
		}
		tools := in.Tools
		if len(tools) == 0 || string(tools) == "null" {
			tools = json.RawMessage("[]") // connect first
		}
		// A name makes a new id here (kiff/tafs#78); an agent id is sent as
		// given, for Setups whose instructions still make their own.
		agent := in.Agent
		if in.Name != "" {
			agent = AgentID(in.Name)
		}
		body, _ := json.Marshal(map[string]any{"server": in.Server, "agent": agent, "tools": tools})
		res, err := t.grant(ctx, http.MethodPost, path, body)
		if err != nil || res.IsError {
			return res, err
		}
		var g grantView
		_ = json.Unmarshal([]byte(res.Text), &g)
		g.Agent = agent
		b, _ := json.Marshal(g)
		return tool.Result{Text: string(b)}, nil
	case "status", "catalog", "tools":
		if in.Grant == "" || strings.ContainsAny(in.Grant, "/?#") {
			return tool.Result{Text: in.Action + " needs the grant's id", IsError: true}, nil
		}
		at := path + "/" + url.PathEscape(in.Grant)
		switch in.Action {
		case "catalog":
			return t.catalog(ctx, at+"/catalog", in.Tool)
		case "tools":
			if len(in.Tools) == 0 || in.Tools[0] != '[' || string(in.Tools) == "[]" {
				return tool.Result{Text: "tools needs the taf's tools, from the grant's catalog", IsError: true}, nil
			}
			body, _ := json.Marshal(map[string]any{"tools": in.Tools})
			return t.grant(ctx, http.MethodPost, at+"/tools", body)
		}
		return t.grant(ctx, http.MethodGet, at, nil)
	}
	// describe is gone (kiff/planque#23): nothing is read from the owner's
	// account before they approve the tools that read it. Ask which mode.
	return tool.Result{Text: "action is request, status, catalog or tools", IsError: true}, nil
}

// AgentID is a new KIFF agent id from the name the model gave: the name as
// an id (lowercase letters, digits and dashes) and a random part, at most 40
// characters. The random part (40 bits) comes from here, not the model,
// which repeats its "random" choices across Setups (kiff/tafs#78).
func AgentID(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if i := strings.IndexRune(accented, r); i >= 0 {
			r = rune(plain[len([]rune(accented[:i]))])
		}
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			dash = false
		default:
			dash = true
		}
	}
	base := b.String()
	if len(base) > 31 {
		base = strings.TrimRight(base[:31], "-")
	}
	if base == "" {
		base = "taf"
	}
	return base + "-" + strings.ToLower(rand.Text()[:8])
}

// accented letters an owner's name for a taf may have, and the plain
// letters an id carries for them, in order.
const (
	accented = "áàâäãåéèêëíìîïóòôöõúùûüñçý"
	plain    = "aaaaaaeeeeiiiiooooouuuuncy"
)

// grantView is what the model reads of a grant: what the owner sees and
// where it stands, nothing more. Agent is set on a request: the taf's KIFF
// agent id, with its random part.
type grantView struct {
	ID      string   `json:"id"`
	Agent   string   `json:"agent,omitempty"`
	Status  string   `json:"status"`
	Text    string   `json:"text,omitempty"`
	Account string   `json:"account,omitempty"`
	Tools   []string `json:"tools,omitempty"`
	Reason  string   `json:"reason,omitempty"`
}

// grant calls planque and answers with the grant it returns.
func (t *Tool) grant(ctx context.Context, method, path string, body []byte) (tool.Result, error) {
	raw, refused, err := t.do(ctx, method, path, body)
	if err != nil || refused != nil {
		return deref(refused), err
	}
	var g grantView
	if err := json.Unmarshal(raw, &g); err != nil || g.ID == "" {
		return tool.Result{}, fmt.Errorf("planque: an answer that isn't a grant")
	}
	b, _ := json.Marshal(g)
	return tool.Result{Text: string(b)}, nil
}

// The catalog goes into the model's context, as the vendor wrote it: it is
// bounded, and says when it was shortened.
const (
	MaxDescription = 1000      // characters of one tool's description
	MaxCatalog     = 100 << 10 // bytes of the whole catalog
)

type catalogTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	ReadOnly    bool            `json:"read_only,omitempty"`
	// Shortened: the description was cut, or the arguments left out to
	// keep the catalog bounded; catalog {grant, tool} shows it in full.
	Shortened bool `json:"shortened,omitempty"`
}

// catalog answers with what the vendor lists for a listed grant: each
// tool's own words and arguments, and the account. No value from the
// account is in it. With only, it is that one tool, in full.
func (t *Tool) catalog(ctx context.Context, path, only string) (tool.Result, error) {
	raw, refused, err := t.do(ctx, http.MethodGet, path, nil)
	if err != nil || refused != nil {
		return deref(refused), err
	}
	var c struct {
		Grant     string        `json:"grant"`
		Server    string        `json:"server"`
		Account   string        `json:"account,omitempty"`
		Tools     []catalogTool `json:"tools"`
		Truncated bool          `json:"truncated,omitempty"`
	}
	if err := json.Unmarshal(raw, &c); err != nil || c.Grant == "" {
		return tool.Result{}, fmt.Errorf("planque: an answer that isn't a catalog")
	}
	if only != "" {
		i := slices.IndexFunc(c.Tools, func(ct catalogTool) bool { return ct.Name == only })
		if i < 0 {
			return tool.Result{Text: "the catalog has no tool named " + only, IsError: true}, nil
		}
		c.Tools = c.Tools[i : i+1]
	} else {
		c.Tools, c.Truncated = bound(c.Tools)
	}
	b, _ := json.Marshal(c)
	if len(b) > MaxCatalog {
		return tool.Result{Text: "this tool's description and arguments are larger than the catalog may be", IsError: true}, nil
	}
	return tool.Result{Text: string(b)}, nil
}

// bound keeps the catalog under MaxCatalog: each description is cut,
// every tool keeps its name and description, and the arguments go to the
// tools they fit, in the vendor's order. Past that, descriptions are cut
// shorter, then the last tools are left out.
func bound(tools []catalogTool) ([]catalogTool, bool) {
	cut := false
	shorten := func(limit int) {
		for i := range tools {
			if r := []rune(tools[i].Description); len(r) > limit {
				tools[i].Description, tools[i].Shortened, cut = string(r[:limit])+"…", true, true
			}
		}
	}
	size := func(t catalogTool) int {
		t.InputSchema = nil
		b, _ := json.Marshal(t)
		return len(b) + 1
	}
	room := MaxCatalog - 1024 // the catalog's own fields
	base := func() int {
		n := 0
		for _, t := range tools {
			n += size(t)
		}
		return n
	}
	shorten(MaxDescription)
	if base() > room {
		shorten(200)
	}
	for base() > room && len(tools) > 1 {
		tools, cut = tools[:len(tools)-1], true
	}
	left := room - base()
	for i := range tools {
		if n := len(tools[i].InputSchema) + len(`,"input_schema":`); n <= left {
			left -= n
		} else if tools[i].InputSchema != nil {
			tools[i].InputSchema, tools[i].Shortened, cut = nil, true, true
		}
	}
	return tools, cut
}

func deref(r *tool.Result) tool.Result {
	if r == nil {
		return tool.Result{}
	}
	return *r
}

// do calls planque with the agent's key. A refusal comes back as a result
// the model can read; err is for planque not answering.
func (t *Tool) do(ctx context.Context, method, path string, body []byte) ([]byte, *tool.Result, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.base+path, rd)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+t.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("planque: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = http.StatusText(resp.StatusCode)
		}
		return nil, &tool.Result{Text: fmt.Sprintf("planque answered %d: %s", resp.StatusCode, e.Error), IsError: true}, nil
	}
	return raw, nil, nil
}
