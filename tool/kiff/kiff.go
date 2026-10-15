// Package kiff is the tool adapter for KIFF's gateway (mcp.kiff.dev): an MCP
// client, over streamable HTTP, that lists the tools the agent's Cards reach
// and calls them with the agent's own KIFF key. The gateway checks every call
// against the Card before anything reaches the tool, and says in each result
// whether it held or refused the call.
//
// The key is sent only as the Authorization header. It is never logged, never
// put in an error, and never recorded in a session.
package kiff

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/tool"
)

// URL is KIFF's gateway.
const URL = "https://mcp.kiff.dev/mcp"

// protocolVersion is the MCP version this client speaks.
const protocolVersion = "2025-06-18"

// maxResponse bounds what one response may hold.
const maxResponse = 8 << 20

// Client talks to the gateway as one agent.
type Client struct {
	url  string
	key  string
	http *http.Client

	mu    sync.Mutex // held while initializing
	ready bool
	next  atomic.Int64

	smu     sync.Mutex
	session string // Mcp-Session-Id, once the gateway gives one
}

// New returns a client for the gateway at url, as the agent whose KIFF key is
// key. A nil hc uses http.DefaultClient.
func New(url, key string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{url: url, key: key, http: hc}
}

// Tools lists the tools the gateway offers this agent. A tool the gateway
// marks readOnlyHint is Read; every other is Write.
func (c *Client) Tools(ctx context.Context) ([]tool.Tool, error) {
	if err := c.init(ctx); err != nil {
		return nil, err
	}
	var out []tool.Tool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
				Annotations *struct {
					ReadOnlyHint bool `json:"readOnlyHint"`
				} `json:"annotations"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", params, &page); err != nil {
			return nil, err
		}
		for _, t := range page.Tools {
			schema := t.InputSchema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			access := tool.Write
			if t.Annotations != nil && t.Annotations.ReadOnlyHint {
				access = tool.Read
			}
			out = append(out, &gatewayTool{c: c, access: access,
				spec: model.ToolSpec{Name: t.Name, Description: t.Description, InputSchema: schema}})
		}
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

type gatewayTool struct {
	c      *Client
	spec   model.ToolSpec
	access tool.Access
}

func (t *gatewayTool) Spec() model.ToolSpec { return t.spec }
func (t *gatewayTool) Access() tool.Access  { return t.access }
func (t *gatewayTool) Gateway() bool        { return true }

// Call calls the tool through the gateway and reads the gateway's state from
// the result's _meta.
func (t *gatewayTool) Call(ctx context.Context, input json.RawMessage) (tool.Result, error) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
		// Meta is kept opaque: the gateway passes other namespaces through
		// from the tool, and only MetaKey is KIFF's to read.
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := t.c.call(ctx, "tools/call", map[string]any{"name": t.spec.Name, "arguments": input}, &res); err != nil {
		return tool.Result{}, err
	}
	var text []string
	for _, c := range res.Content {
		if c.Type == "text" {
			text = append(text, c.Text)
		}
	}
	var m meta
	if raw, ok := res.Meta[MetaKey]; ok {
		if err := json.Unmarshal(raw, &m); err != nil {
			return tool.Result{}, fmt.Errorf("kiff: the gateway's %s: %w", MetaKey, err)
		}
	}
	out := tool.Result{Text: strings.Join(text, "\n"), IsError: res.IsError, State: m.State}
	if m.State == tool.StateHeld {
		out.Hold = &tool.Hold{Operation: m.OperationID, Exception: m.ExceptionID}
		if m.HoldExpiresAt != nil {
			out.Hold.Expires = m.HoldExpiresAt.UTC()
		}
	}
	return out, nil
}

// MetaKey is the result _meta key where the gateway says what happened to a
// call it wrote the result of.
const MetaKey = "dev.kiff/call"

// meta is what this client reads at MetaKey.
type meta struct {
	State         string     `json:"state"`
	OperationID   string     `json:"operation_id"`
	ExceptionID   string     `json:"exception_id"`
	HoldExpiresAt *time.Time `json:"hold_expires_at"`
}

// init opens the MCP session once.
func (c *Client) init(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ready {
		return nil
	}
	params := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "kiff-agents", "version": "0"},
	}
	if err := c.post(ctx, "initialize", params, true, nil); err != nil {
		return err
	}
	if err := c.post(ctx, "notifications/initialized", nil, false, nil); err != nil {
		return err
	}
	c.ready = true
	return nil
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	if err := c.init(ctx); err != nil {
		return err
	}
	return c.post(ctx, method, params, true, out)
}

// Error is a JSON-RPC error the gateway answered with.
type Error struct {
	Method  string
	Code    int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("kiff: %s: %s (%d)", e.Method, e.Message, e.Code) }

// post sends one JSON-RPC message. A request (withID) waits for its response,
// as JSON or as a server-sent event stream; a notification wants a 2xx only.
func (c *Client) post(ctx context.Context, method string, params any, withID bool, out any) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	var id int64
	if withID {
		id = c.next.Add(1)
		msg["id"] = id
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	c.smu.Lock()
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	c.smu.Unlock()
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("kiff: %s: %w", method, scrub(err, c.key))
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.smu.Lock()
		c.session = sid
		c.smu.Unlock()
	}
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("kiff: %s: HTTP %d: %s", method, resp.StatusCode, c.redact(strings.TrimSpace(string(snippet))))
	}
	if !withID {
		return nil
	}
	raw, err := readResponse(resp, id)
	if err != nil {
		return fmt.Errorf("kiff: %s: %w", method, scrub(err, c.key))
	}
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &rpc); err != nil {
		return fmt.Errorf("kiff: %s: %w", method, err)
	}
	if rpc.Error != nil {
		return &Error{Method: method, Code: rpc.Error.Code, Message: c.redact(rpc.Error.Message)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(rpc.Result, out)
}

// readResponse returns the JSON-RPC response with id, from a JSON body or an
// event stream.
func readResponse(resp *http.Response, id int64) (json.RawMessage, error) {
	r := io.LimitReader(resp.Body, maxResponse)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return io.ReadAll(r)
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxResponse)
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "" && data.Len() > 0:
			msg := json.RawMessage(data.String())
			data.Reset()
			var head struct {
				ID *int64 `json:"id"`
			}
			if json.Unmarshal(msg, &head) == nil && head.ID != nil && *head.ID == id {
				return msg, nil
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("the event stream ended without a response")
}

// redact keeps the key out of text the gateway sent back, which reaches
// errors, tool results and the model.
func (c *Client) redact(s string) string {
	if c.key == "" {
		return s
	}
	return strings.ReplaceAll(s, c.key, "[key]")
}

// scrub keeps the key out of an error's text.
func scrub(err error, key string) error {
	if key == "" || !strings.Contains(err.Error(), key) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), key, "[key]"))
}
