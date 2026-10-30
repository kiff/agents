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
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/kiff/agents/run"
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
	// trials is whether the gateway said it decides a call without sending
	// it (TrialCapability). A trial's call goes to a gateway that said so,
	// or nowhere: one that didn't would send it.
	trials bool
	next   atomic.Int64

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
		if err := c.read(ctx, "tools/list", params, true, &page); err != nil {
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

// Call calls the account's tool name through the gateway, as this agent,
// whether or not the gateway lists it to the agent: KIFF decides it on the
// agent's Card all the same (Setup's reads, kiff/tafs#138).
func (c *Client) Call(ctx context.Context, name string, input json.RawMessage) (tool.Result, error) {
	if err := c.init(ctx); err != nil {
		return tool.Result{}, err
	}
	return (&gatewayTool{c: c, access: tool.Read, spec: model.ToolSpec{Name: name}}).Call(ctx, input)
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
	out, _, err := t.callMeta(ctx, input)
	return out, err
}

// callMeta is Call, with what the gateway said in MetaKey.
func (t *gatewayTool) callMeta(ctx context.Context, input json.RawMessage) (tool.Result, meta, error) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	// A run's call goes with its own operation ID, the same each time the
	// call is made: after a crash, KIFF gives its first outcome back rather
	// than act twice. A trial's call is only decided, never sent, so it needs
	// none; a call made outside a run's loop (Collect) brings its own.
	if ref, ok := run.RefFrom(ctx); ok && ref.Run != "" {
		if id := tool.CallFrom(ctx); id != "" {
			if _, trial := run.TrialFrom(ctx); !trial {
				with, err := withOperation(input, Operation(ref.Run, id))
				if err != nil {
					return tool.Result{}, meta{}, fmt.Errorf("kiff: call %s: %w", id, err)
				}
				input = with
			}
		}
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
		return tool.Result{}, meta{}, err
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
			return tool.Result{}, meta{}, fmt.Errorf("kiff: the gateway's %s: %w", MetaKey, err)
		}
	}
	out := tool.Result{Text: strings.Join(text, "\n"), IsError: res.IsError, State: m.State, Invocation: str(m.Invocation)}
	// What the call is on: the argument KIFF's Card scopes on, where KIFF
	// says it is in the call's arguments (never a guess), and only a string.
	if name := str(m.ResourceArgument); name != "" {
		path := str(m.ResourcePath)
		if path == "" {
			path = name
		}
		out.Resource = str(atPath(input, path))
	}
	if _, trial := run.TrialFrom(ctx); trial {
		// A trial never pauses: KIFF answers its writes with a decision. It
		// sends nothing, so there's nothing for the Record to name.
		out.Trial, out.State, out.Resource = m.Trial, "", ""
		return out, m, nil
	}
	if m.State == tool.StateHeld {
		// Only state says whether the call is held. The hold's fields are
		// read best effort: one KIFF writes in another shape is left empty,
		// and an empty expiry fails closed, so the run still pauses.
		// A call made without kiff_operation_id is keyed by KIFF's own
		// "tc-…" key, which the gateway takes as kiff_operation_id too.
		op := str(m.OperationID)
		if op == "" {
			op = str(m.Invocation)
		}
		out.Hold = &tool.Hold{Operation: op, Exception: str(m.ExceptionID)}
		var at string
		if json.Unmarshal(m.HoldExpiresAt, &at) == nil {
			if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
				out.Hold.Expires = t.UTC()
			}
		}
	}
	return out, m, nil
}

// Operation is the kiff_operation_id of the model's call callID in run: the
// same every time that call is made, and no other call's.
func Operation(run, callID string) string {
	sum := sha256.Sum256([]byte(run + "\x00" + callID))
	return "op-" + hex.EncodeToString(sum[:])[:40]
}

// atPath is the value at a dotted path in a call's arguments
// ("parameters.charge"), as KIFF's gateway reads it (lookupPath): each segment
// a key of an object. Nil when the path isn't there.
func atPath(input json.RawMessage, path string) json.RawMessage {
	cur := input
	for _, seg := range strings.Split(path, ".") {
		if seg == "" {
			return nil
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(cur, &obj) != nil {
			return nil
		}
		next, ok := obj[seg]
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

// str is raw as a JSON string, or "" when it isn't one.
func str(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// MetaKey is the result _meta key where the gateway says what happened to a
// call it wrote the result of.
const MetaKey = "dev.kiff/call"

// meta is what this client reads at MetaKey.
type meta struct {
	State string `json:"state"`
	// Retry "same_call" means the call has no final outcome yet: repeating
	// it gets the answer.
	Retry      string          `json:"retry"`
	Invocation json.RawMessage `json:"invocation"`
	// ResourceArgument names the argument the call's Card scopes on (the
	// gateway tool's resource argument): its name, never its value.
	ResourceArgument json.RawMessage `json:"resource_argument"`
	// ResourcePath is where that argument's value is in the call's
	// arguments, dotted ("parameters.charge"): the name itself when it's at
	// the top level (a sealed connection's resource_path).
	ResourcePath  json.RawMessage `json:"resource_path"`
	OperationID   json.RawMessage `json:"operation_id"`
	ExceptionID   json.RawMessage `json:"exception_id"`
	HoldExpiresAt json.RawMessage `json:"hold_expires_at"`
	// Trial is KIFF's decision on a trial's write, which it never sent
	// (kiff/tafs#107): only for a call made with KIFF-Trial.
	Trial *tool.TrialDecision `json:"trial"`
}

// TrialHeader asks KIFF to decide a call and not send it (kiff/tafs#107):
// its value is the draft whose Cards decide, or "live". Set from the run's
// trial mark only, never from anything the model wrote.
const TrialHeader = "KIFF-Trial"

// TrialRunHeader names the trial's run, from the same mark: KIFF counts
// the trial's earlier allowed writes toward the Cards' limits, and
// refuses a trial's write that names none.
const TrialRunHeader = "KIFF-Trial-Run"

// RunGrantHeader carries the run's grant on each of its calls: the person
// KIFF signed it for asked, so KIFF records the call, and any hold it
// opens, as theirs. Set from the stored run only, never from the model.
const RunGrantHeader = "KIFF-Run-Grant"

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
	var res struct {
		Capabilities struct {
			Experimental map[string]json.RawMessage `json:"experimental"`
		} `json:"capabilities"`
	}
	if err := c.read(ctx, "initialize", params, true, &res); err != nil {
		return err
	}
	var trials bool
	c.trials = json.Unmarshal(res.Capabilities.Experimental[TrialCapability], &trials) == nil && trials
	if err := c.read(ctx, "notifications/initialized", nil, false, nil); err != nil {
		return err
	}
	c.ready = true
	return nil
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	if err := c.init(ctx); err != nil {
		return err
	}
	if _, trial := run.TrialFrom(ctx); trial && method == "tools/call" && !c.trials {
		return ErrNoTrials
	}
	return c.post(ctx, method, params, true, out)
}

// ReadRetryWait is how long a read waits before its one retry after the
// gateway answered 502.
var ReadRetryWait = time.Second

// read posts a message that only reads (opening the session, listing tools)
// and, when the gateway answers 502 (a deploy, a cold start), tries once
// more. An action (tools/call) is never retried here: the gateway may have
// sent it already.
func (c *Client) read(ctx context.Context, method string, params any, withID bool, out any) error {
	err := c.post(ctx, method, params, withID, out)
	var h *HTTPError
	if !errors.As(err, &h) || h.Status != http.StatusBadGateway {
		return err
	}
	select {
	case <-ctx.Done():
		return err
	case <-time.After(ReadRetryWait):
	}
	return c.post(ctx, method, params, withID, out)
}

// HTTPError is a gateway answer that wasn't 2xx.
type HTTPError struct {
	Method string
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("kiff: %s: HTTP %d: %s", e.Method, e.Status, e.Body)
}

// TrialCapability is the experimental capability a gateway that decides
// trials (KIFF-Trial) declares in its initialize answer, as true.
const TrialCapability = "dev.kiff/trial"

// ErrNoTrials is a trial's call to a gateway that hasn't said it decides
// trials: it was not sent, since that gateway would send it for real.
var ErrNoTrials = errors.New("kiff: this gateway doesn't run trials yet; the call was not sent")

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
	if t, ok := run.TrialFrom(ctx); ok {
		draft := t.Draft
		if draft == "" {
			draft = "live"
		}
		req.Header.Set(TrialHeader, draft)
		if t.Run != "" {
			req.Header.Set(TrialRunHeader, t.Run)
		}
	}
	if g := run.GrantFrom(ctx); g != "" {
		req.Header.Set(RunGrantHeader, g)
	}
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
		return &HTTPError{Method: method, Status: resp.StatusCode, Body: c.redact(strings.TrimSpace(string(snippet)))}
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
