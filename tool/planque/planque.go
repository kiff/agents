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
	// reads calls a setup_reads tool through KIFF's gateway, with the
	// agent's own key (kiff/tafs#138, gate A). Nil: no read is made.
	reads Reader
}

// Reader calls one of the account's tools through KIFF's gateway, as the
// agent: KIFF decides it on the agent's Card, issues the permit planque
// sends it with, and keeps its receipt and its line in the Record.
type Reader interface {
	Call(ctx context.Context, name string, input json.RawMessage) (tool.Result, error)
}

// ReadThrough has the agent's reads (setup_reads) go through KIFF's gateway
// at r, and returns t.
func (t *Tool) ReadThrough(r Reader) *Tool {
	t.reads = r
	return t
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

// Reads reports whether a call only reads: status, catalog, read and grants
// ask planque what is there; request and tools ask for something.
func (t *Tool) Reads(input json.RawMessage) bool {
	var in struct {
		Action string `json:"action"`
	}
	_ = json.Unmarshal(input, &in)
	return in.Action == "status" || in.Action == "catalog" || in.Action == "read" || in.Action == "grants"
}

func (t *Tool) Spec() model.ToolSpec {
	return model.ToolSpec{
		Name: Name,
		Description: "Ask planque for access to a vendor account for a taf, and follow the request. planque keeps the vendor's sign-in; you never see a token. " +
			"To look before you choose: request {server, agent} with no tools connects first. Once the owner approved and signed in, the grant is listed, " +
			"and catalog {grant} shows the vendor's tools (name, description, arguments, read_only) and the account the sign-in acts in; nothing is called. " +
			"The catalog follows vendor pages and lists tools by name. If next_cursor is present, catalog {grant, cursor: next_cursor} continues the list; keep going to find tools on later pages. In a large catalog some tools are shortened (shortened: true: arguments or description left out); catalog {grant, tool} searches the pages for one in full, so ask for the ones you need. " +
			"Then tools {grant, tools} sets the taf's tools from that catalog, and the owner approves them, with no second sign-in. " +
			"On a connected grant, tools {grant, tools} adds the tools sent (only those to add) to what it has: the owner approves the addition once, " +
			"with no sign-in, and its tools keep working meanwhile. So a taf has one connection per vendor: grants {agent} lists the connections you asked for a taf's agent " +
			"(id, agent, server, status, created, tools); add to the one for that vendor rather than requesting another. " +
			"grants with no agent lists every connection you asked for on the account, each with its agent: when you no longer have a taf's agent id " +
			"or a grant's id, find it there, never ask for a new connection or the owner to sign in again. " +
			"request {server, agent, tools} asks for known tools at once. server is the vendor's MCP server URL. " +
			"A vendor with a REST API and no MCP server is reached by its key instead: request {api: {url, base, auth: {in, name}}, agent, tools}, " +
			"where url is the vendor's published OpenAPI description (JSON), base the URL its operations' paths start from, and auth where the key goes " +
			"(in: header or query, name: the header or parameter the vendor's docs name). The owner gives the key on their phone, with the approval, straight to planque; never ask for it. " +
			"name a short name for a new taf's KIFF agent, from the taf's name (e.g. support): the tool makes the agent's id from it, with a random part so it is new, " +
			"and the answer's agent is that id, to use from then on (its draft, its result); agent, instead of name, is an id already made, sent as given. Each tool is {tool, name?, title, read_only?, setup_reads?, amount_argument?, amount_unit?, amount_scale?, resource_argument?, unit_lookup?, fixed_arguments?, arguments?}: " +
			"title says what the tool does, in the owner's words and language, on one line (e.g. Reply to a ticket, Read an event): the owner's approval, KIFF's Card and the record name the tool by it, never by its name. " +
			"fixed_arguments pins a vendor argument (by dotted path) to a value; arguments are then the only ones the taf may give, each {path, type, required?}. " +
			"Every argument's path is a field the vendor's operation takes, read from the vendor's own description of it (its OpenAPI description, or the vendor tool that describes an operation's fields), never made up: " +
			"to narrow a broad tool (one that takes an operation's name and its parameters) to one operation, connect the describing tool first, with setup_reads, and add the write after the owner approved it. " +
			"amount_argument names only the argument the operation takes for the money it moves, in its own units; a write that moves no money (posting a reply, moving a meeting, sharing a file) has none, and its Card limits its calls. " +
			"resource_argument names the string argument the call acts on (the ticket, the event, the payment), one the taf's read of it takes. " +
			"unit_lookup {tool, argument, path} names, for a tool that moves money, the read that says its unit: " +
			"the read's name (not its vendor tool), its argument that takes the same id, and where the unit is in its answer; KIFF refuses a call in another unit than the Card's. " +
			"When the vendor has both test and live, the grant says which the sign-in acts in; set up in test first when the owner has it. " +
			`e.g. {"tool":"post_reply","title":"Reply to a ticket","resource_argument":"ticket","fixed_arguments":{"workspace":"w_1"},` +
			`"arguments":{"ticket":{"path":"ticket","type":"string","required":true},"body":{"path":"body","type":"string","required":true}}} ` +
			"pins the workspace and makes the ticket and the reply required, so no reply leaves them out or goes elsewhere. " +
			"Each answer is the grant's id, status and the text the owner will read (each tool by its title; its exact terms are under Details). Asking grants nothing: show an approval with the grant id. " +
			"setup_reads: true, on a read-only tool, lets you read it yourself once the owner approved it: read {grant, tool, arguments}, with the name the grant lists. " +
			"Use it for a vendor's tool that lists an operation's fields, so you name each argument's path from the vendor, never by guess. " +
			"status {grant}: pending, approved, signed_in, listed (signed in, tools listed, none given yet), connected (the tools are in KIFF), " +
			"failed (with a reason) or disconnected. Only connected means the taf can use the tools; the owner's word in the chat is not enough.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"action":{"type":"string","enum":["request","status","catalog","tools","read","grants"]},` +
			`"server":{"type":"string","description":"the vendor's MCP server URL (request)"},` +
			`"api":{"type":"object","description":"a REST vendor reached by its key, instead of server (request)","properties":{` +
			`"url":{"type":"string","description":"the vendor's OpenAPI description (JSON)"},"base":{"type":"string","description":"the URL its operations' paths start from"},` +
			`"auth":{"type":"object","properties":{"in":{"type":"string","enum":["header","query"]},"name":{"type":"string"}},"required":["in","name"]}},"required":["url","base","auth"]},` +
			`"name":{"type":"string","description":"a short name for a new taf's KIFF agent, from the taf's name (request); the answer's agent is its id"},` +
			`"agent":{"type":"string","description":"the taf's KIFF agent id, already made (request; grants, where none lists every connection you asked for); not with name"},` +
			`"tools":{"type":"array","description":"the vendor tools to grant (request, tools); none to connect first","items":{"type":"object"}},` +
			`"grant":{"type":"string","description":"the grant's id (status, catalog, tools)"},` +
			`"tool":{"type":"string","description":"one tool of the catalog, to read it in full (catalog); a setup_reads tool, by the name the grant lists (read)"},` +
			`"cursor":{"type":"string","description":"the previous catalog answer's next_cursor, to continue listing or searching (catalog)"},` +
			`"arguments":{"type":"object","description":"the read's arguments (read)"}},` +
			`"required":["action"]}`),
	}
}

// Call asks for a grant or reads one. What planque refuses comes back as a
// result the model can read and correct.
func (t *Tool) Call(ctx context.Context, input json.RawMessage) (tool.Result, error) {
	var in struct {
		Action string          `json:"action"`
		Server string          `json:"server"`
		API    json.RawMessage `json:"api"`
		Agent  string          `json:"agent"`
		Name   string          `json:"name"`
		Tools  json.RawMessage `json:"tools"`
		Grant  string          `json:"grant"`
		Tool   string          `json:"tool"`
		Cursor string          `json:"cursor"`
		Args   json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return tool.Result{Text: "the input must be {action, …}", IsError: true}, nil
	}
	path := "/v1/accounts/" + url.PathEscape(t.tenant) + "/grants"
	switch in.Action {
	case "grants":
		// The connections this agent asked for a taf's agent, so an
		// improvement adds to one instead of asking for another (kiff/tafs#138).
		// With no agent, every connection it asked for, each naming its agent:
		// a Setup that lost a taf's agent id finds it again (kiff/planque#49).
		at := "/v1/accounts/" + url.PathEscape(t.tenant) + "/asked-grants"
		if in.Agent != "" {
			if strings.ContainsAny(in.Agent, "/?#") {
				return tool.Result{Text: "grants takes the taf's agent id, or none for every connection you asked for", IsError: true}, nil
			}
			at = "/v1/accounts/" + url.PathEscape(t.tenant) + "/agents/" + url.PathEscape(in.Agent) + "/grants"
		}
		raw, refused, err := t.do(ctx, http.MethodGet, at, nil)
		if err != nil || refused != nil {
			return deref(refused), err
		}
		return tool.Result{Text: string(raw)}, nil
	case "request":
		byKey := len(in.API) > 0 && string(in.API) != "null"
		if (in.Server == "") == !byKey || (in.Agent == "") == (in.Name == "") {
			return tool.Result{Text: "request needs server (an MCP server) or api (a REST vendor by its key), not both, and either name (a new taf's agent) or agent (an id already made); and tools, unless you connect first", IsError: true}, nil
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
		ask := map[string]any{"server": in.Server, "agent": agent, "tools": tools}
		if byKey {
			// planque reads the description into a snapshot the owner approves,
			// and takes the key from the owner's phone (kiff/planque#64).
			ask = map[string]any{"kind": "api_key", "api": in.API, "agent": agent, "tools": tools}
		}
		body, _ := json.Marshal(ask)
		res, err := t.grant(ctx, http.MethodPost, path, body)
		if err != nil || res.IsError {
			return res, err
		}
		var g grantView
		_ = json.Unmarshal([]byte(res.Text), &g)
		g.Agent = agent
		b, _ := json.Marshal(g)
		return tool.Result{Text: string(b)}, nil
	case "status", "catalog", "tools", "read":
		if in.Grant == "" || strings.ContainsAny(in.Grant, "/?#") {
			return tool.Result{Text: in.Action + " needs the grant's id", IsError: true}, nil
		}
		at := path + "/" + url.PathEscape(in.Grant)
		switch in.Action {
		case "catalog":
			return t.catalog(ctx, at+"/catalog", in.Tool, in.Cursor)
		case "read":
			// A tool the owner approved for Setup to read (setup_reads), by
			// the name the grant lists (kiff/tafs#174). It goes through KIFF's
			// gateway like any call, decided on Setup's own read Card, so it
			// has a permit, a receipt and a line in the Record (kiff/tafs#138).
			if in.Tool == "" {
				return tool.Result{Text: "read needs the tool, by the name the grant lists, and its arguments", IsError: true}, nil
			}
			if t.reads == nil {
				return tool.Result{Text: "reads go through KIFF's gateway, and none is set up here", IsError: true}, nil
			}
			args := in.Args
			if len(args) == 0 || string(args) == "null" {
				args = json.RawMessage("{}")
			}
			return t.read(ctx, in.Tool, args)
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
	return tool.Result{Text: "action is request, status, catalog, tools, read or grants", IsError: true}, nil
}

// MaxStated is how many of a session's grants State looks up: the latest.
const MaxStated = 8

// State is where each grant the session's components name stands now, as
// planque says: what the agent would read with status, without a turn.
func (t *Tool) State(ctx context.Context, shown []json.RawMessage) (string, error) {
	grants := tool.Named(shown, "grant", MaxStated)
	if len(grants) == 0 {
		return "", nil
	}
	path := "/v1/accounts/" + url.PathEscape(t.tenant) + "/grants/"
	var b strings.Builder
	b.WriteString("planque's grants in this conversation, as planque says now (status {grant} gives the same):\n")
	for _, id := range grants {
		if strings.ContainsAny(id, "/?#") {
			continue
		}
		res, err := t.grant(ctx, http.MethodGet, path+url.PathEscape(id), nil)
		switch {
		case err != nil:
			fmt.Fprintf(&b, "- %s: planque didn't answer now\n", id)
		case res.IsError:
			fmt.Fprintf(&b, "- %s: %s\n", id, res.Text)
		default:
			fmt.Fprintf(&b, "- %s\n", res.Text)
		}
	}
	return b.String(), nil
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

// read answers with the vendor's own answer to a setup_reads read, through
// KIFF's gateway: its text, bounded like the catalog, and whether it was an
// error (the vendor's, or KIFF's refusal).
func (t *Tool) read(ctx context.Context, name string, args json.RawMessage) (tool.Result, error) {
	res, err := t.reads.Call(ctx, name, args)
	if err != nil {
		return tool.Result{}, err
	}
	text := res.Text
	if len(text) > MaxCatalog {
		text = text[:MaxCatalog] + " …(shortened)"
	}
	return tool.Result{Text: text, IsError: res.IsError}, nil
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

// bound keeps the catalog under MaxCatalog: each description is cut, and
// the arguments go to the tools they fit, in the vendor's order. Past
// that, descriptions are cut shorter, then the last tools keep their name
// alone. Every tool keeps its name, so the model can ask for any of them in
// full; only names past MaxCatalog themselves are left out. It answers the
// tools, how many of them are shortened, and how many are left out.
func bound(tools []catalogTool, room int) ([]catalogTool, int, int) {
	shorten := func(from, limit int) {
		for i := from; i < len(tools); i++ {
			if r := []rune(tools[i].Description); len(r) > limit {
				tools[i].Description, tools[i].Shortened = string(r[:limit])+"…", true
			}
		}
	}
	size := func(t catalogTool) int {
		if t.InputSchema != nil {
			t.Shortened = true // reserve the marker if its arguments don't fit
		}
		t.InputSchema = nil
		b, _ := json.Marshal(t)
		return len(b) + 1
	}
	base := func() int {
		n := 0
		for _, t := range tools {
			n += size(t)
		}
		return n
	}
	shorten(0, MaxDescription)
	if base() > room {
		shorten(0, 200)
	}
	// The last tools lose their description, one at a time, before any loses its name.
	used := base()
	for i := len(tools) - 1; i >= 0 && used > room; i-- {
		if tools[i].Description != "" {
			before := size(tools[i])
			tools[i].Description, tools[i].Shortened = "", true
			used += size(tools[i]) - before
		}
	}
	left := 0
	for used > room && len(tools) > 1 {
		used -= size(tools[len(tools)-1])
		tools, left = tools[:len(tools)-1], left+1
	}
	free := room - used
	for i := range tools {
		full, _ := json.Marshal(tools[i])
		if n := len(full) + 1 - size(tools[i]); n <= free {
			free -= n
		} else if tools[i].InputSchema != nil {
			tools[i].InputSchema, tools[i].Shortened = nil, true
		}
	}
	shortened := 0
	for _, t := range tools {
		if t.Shortened {
			shortened++
		}
	}
	return tools, shortened, left
}

// catalogNote says how complete a bounded catalog is, in the words the
// model acts on: what is listed, and how to read a shortened tool whole.
func catalogNote(listed, shortened, left int, truncated bool) string {
	switch {
	case left > 0 || truncated:
		return fmt.Sprintf("%d tools are listed; the vendor has more that didn't fit. %d listed tools are shortened: catalog {grant, tool} shows one in full.", listed, shortened)
	case shortened > 0:
		return fmt.Sprintf("Every tool is listed (%d). %d are shortened, with arguments or description left out: catalog {grant, tool} shows one in full.", listed, shortened)
	default:
		return ""
	}
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
	return t.doBounded(ctx, method, path, body, 1<<20)
}

func (t *Tool) doBounded(ctx context.Context, method, path string, body []byte, limit int64) ([]byte, *tool.Result, error) {
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
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, nil, fmt.Errorf("planque: reading answer: %w", err)
	}
	if int64(len(raw)) > limit {
		return nil, nil, fmt.Errorf("planque: answer exceeds %d bytes", limit)
	}
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
