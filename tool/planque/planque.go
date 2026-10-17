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
			"request {server, agent, tools}: server is the vendor's MCP server URL (e.g. https://mcp.stripe.com), agent the taf's KIFF agent id, " +
			"tools the vendor tools to grant, each {tool, name?, read_only?, amount_argument?, amount_unit?, amount_scale?, fixed_arguments?, arguments?}; " +
			"it returns the grant's id, status and the text the owner will read. Asking grants nothing: show an approval with the grant id. " +
			"status {grant}: the grant's status now: pending, approved, signed_in, connected (the tools are in KIFF), failed (with a reason) or disconnected. " +
			"Only status connected means the taf can use the tools; the owner's word in the chat is not enough.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"action":{"type":"string","enum":["request","status"]},` +
			`"server":{"type":"string","description":"the vendor's MCP server URL (request)"},` +
			`"agent":{"type":"string","description":"the taf's KIFF agent id (request)"},` +
			`"tools":{"type":"array","description":"the vendor tools to grant (request)","items":{"type":"object"}},` +
			`"grant":{"type":"string","description":"the grant's id (status)"}},` +
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
		Tools  json.RawMessage `json:"tools"`
		Grant  string          `json:"grant"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return tool.Result{Text: "the input must be {action, …}", IsError: true}, nil
	}
	path := "/v1/accounts/" + url.PathEscape(t.tenant) + "/grants"
	switch in.Action {
	case "request":
		if in.Server == "" || in.Agent == "" || len(in.Tools) == 0 {
			return tool.Result{Text: "request needs server, agent and tools", IsError: true}, nil
		}
		body, _ := json.Marshal(map[string]any{"server": in.Server, "agent": in.Agent, "tools": in.Tools})
		return t.do(ctx, http.MethodPost, path, body)
	case "status":
		if in.Grant == "" || strings.ContainsAny(in.Grant, "/?#") {
			return tool.Result{Text: "status needs the grant's id", IsError: true}, nil
		}
		return t.do(ctx, http.MethodGet, path+"/"+url.PathEscape(in.Grant), nil)
	}
	return tool.Result{Text: "action is request or status", IsError: true}, nil
}

// grantView is what the model reads of a grant: what the owner sees and
// where it stands, nothing more.
type grantView struct {
	ID     string   `json:"id"`
	Status string   `json:"status"`
	Text   string   `json:"text,omitempty"`
	Tools  []string `json:"tools,omitempty"`
	Reason string   `json:"reason,omitempty"`
}

func (t *Tool) do(ctx context.Context, method, path string, body []byte) (tool.Result, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.base+path, rd)
	if err != nil {
		return tool.Result{}, err
	}
	req.Header.Set("Authorization", "Bearer "+t.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return tool.Result{}, fmt.Errorf("planque: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = http.StatusText(resp.StatusCode)
		}
		return tool.Result{Text: fmt.Sprintf("planque answered %d: %s", resp.StatusCode, e.Error), IsError: true}, nil
	}
	var g grantView
	if err := json.Unmarshal(raw, &g); err != nil || g.ID == "" {
		return tool.Result{}, fmt.Errorf("planque: an answer that isn't a grant (%d)", resp.StatusCode)
	}
	b, _ := json.Marshal(g)
	return tool.Result{Text: string(b)}, nil
}
