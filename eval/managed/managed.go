// Package managed reads today's tafs runs, which run on Claude Managed
// Agents, as session events, so the evals grade them the way they grade this
// repository's runtime. It's the baseline: what "as good" is measured against.
//
// Only what a check reads is kept: messages, tool calls and results, errors,
// and one Usage event with the session's tokens and list cost. Thinking,
// spans and status changes are left out.
package managed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kiff/agents/eval"
	agentrun "github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
)

// Beta is the Managed Agents beta the reads ask for.
const Beta = "managed-agents-2026-04-01"

// DefaultURL is Anthropic's API.
const DefaultURL = "https://api.anthropic.com"

// Client reads sessions from the Managed Agents API.
type Client struct {
	url  string
	key  string
	http *http.Client
}

// New returns a client for the API at base (DefaultURL if empty) with key. hc
// may be nil.
func New(base, key string, hc *http.Client) *Client {
	if base == "" {
		base = DefaultURL
	}
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{url: strings.TrimRight(base, "/"), key: key, http: hc}
}

// Read returns a session as a graded run.
func (c *Client) Read(ctx context.Context, id string) (eval.Run, error) {
	var s Session
	if err := c.get(ctx, "/v1/sessions/"+url.PathEscape(id), nil, &s); err != nil {
		return eval.Run{}, err
	}
	var events []Event
	page := ""
	for {
		q := url.Values{"order": {"asc"}, "limit": {"100"}}
		if page != "" {
			q.Set("page", page)
		}
		var p struct {
			Data     []Event `json:"data"`
			NextPage string  `json:"next_page"`
		}
		if err := c.get(ctx, "/v1/sessions/"+url.PathEscape(id)+"/events", q, &p); err != nil {
			return eval.Run{}, err
		}
		events = append(events, p.Data...)
		if p.NextPage == "" || p.NextPage == page {
			break
		}
		page = p.NextPage
	}
	return Map(s, events)
}

// Error is an answer from the API other than 200. It carries the API's own
// message, never the request.
type Error struct {
	Status  int
	Path    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("managed: %s: %d %s", e.Path, e.Status, e.Message)
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := c.url + path + "?beta=true"
	if len(q) > 0 {
		u += "&" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", c.key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", Beta)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("managed: %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("managed: %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		return &Error{Status: resp.StatusCode, Path: path, Message: e.Error.Message}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("managed: %s: %w", path, err)
	}
	return nil
}

// Session is the part of a Managed Agents session the baseline reads.
type Session struct {
	ID           string            `json:"id"`
	Status       string            `json:"status"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
	DeploymentID string            `json:"deployment_id"`
	Metadata     map[string]string `json:"metadata"`
	Agent        struct {
		Model json.RawMessage `json:"model"` // {"id": ...} or a bare id
	} `json:"agent"`
	Usage Usage `json:"usage"`
}

// Usage is a session's cumulative usage. ListCost is in cents, as a decimal
// string.
type Usage struct {
	InputTokens   int64 `json:"input_tokens"`
	OutputTokens  int64 `json:"output_tokens"`
	CacheRead     int64 `json:"cache_read_input_tokens"`
	CacheCreation struct {
		OneHour  int64 `json:"ephemeral_1h_input_tokens"`
		FiveMins int64 `json:"ephemeral_5m_input_tokens"`
	} `json:"cache_creation"`
	ListCost *struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	} `json:"list_cost"`
}

// Event is one Managed Agents session event; which fields are set depends on
// Type.
type Event struct {
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	ProcessedAt *time.Time      `json:"processed_at"`
	Content     []Block         `json:"content"`
	Name        string          `json:"name"`
	Server      string          `json:"mcp_server_name"`
	Input       json.RawMessage `json:"input"`
	UseID       string          `json:"mcp_tool_use_id"`
	ToolUseID   string          `json:"tool_use_id"`
	IsError     bool            `json:"is_error"`
	Error       *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	Usage *Usage `json:"usage"`
}

// Block is a content block. Only text is read: from text blocks, plain-text
// documents and search results.
type Block struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Source *struct {
		Type string `json:"type"`
		Data string `json:"data"`
	} `json:"source"`
	Content []Block `json:"content"`
}

func text(blocks []Block) string {
	var parts []string
	for _, b := range blocks {
		switch {
		case b.Type == "text":
			parts = append(parts, b.Text)
		case b.Type == "document" && b.Source != nil && b.Source.Type == "text":
			parts = append(parts, b.Source.Data)
		case b.Type == "search_result":
			parts = append(parts, text(b.Content))
		}
	}
	return strings.Join(parts, "\n")
}

// Map turns a session and its events, oldest first, into a run. A run's last
// agent message is its report.
func Map(s Session, events []Event) (eval.Run, error) {
	kind := session.KindChat
	if s.DeploymentID != "" {
		kind = session.KindRun
	}
	status := agentrun.Running
	if s.Status == "idle" || s.Status == "terminated" {
		status = agentrun.Completed
	}
	run := eval.Run{Session: session.Session{
		ID: s.ID, Agent: s.Metadata["tafs_job"], Kind: kind,
		Started: s.CreatedAt.UTC(), Updated: s.UpdatedAt.UTC(),
	}, Status: status}

	at := s.CreatedAt.UTC()
	tools := map[string]string{} // tool use id → tool name
	var latest *Usage            // the newest usage snapshot among the events
	report := -1
	add := func(k session.EventKind, body any) error {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		run.Events = append(run.Events, session.Event{
			Session: s.ID, Run: s.ID, Seq: int64(len(run.Events) + 1), Time: at, Kind: k, Body: raw,
		})
		return nil
	}
	for _, e := range events {
		if e.ProcessedAt != nil {
			at = e.ProcessedAt.UTC()
		}
		var err error
		switch e.Type {
		case "user.message":
			err = add(session.OwnerMessage, eval.TextBody{Text: text(e.Content)})
		case "agent.message":
			report = len(run.Events)
			err = add(session.AgentMessage, eval.TextBody{Text: text(e.Content)})
		case "agent.mcp_tool_use", "agent.tool_use", "agent.custom_tool_use":
			tools[e.ID] = e.Name
			err = add(session.ToolCall, eval.ToolCallBody{CallID: e.ID, Tool: e.Name, Server: e.Server, Input: orEmpty(e.Input)})
		case "agent.mcp_tool_result", "agent.tool_result", "user.tool_result", "user.custom_tool_result":
			id := e.UseID
			if id == "" {
				id = e.ToolUseID
			}
			err = add(session.ToolResult, eval.ToolResultBody{CallID: id, Tool: tools[id], Text: text(e.Content), IsError: e.IsError})
		case "session.error":
			msg := "unknown error"
			if e.Error != nil {
				msg = e.Error.Type + ": " + e.Error.Message
				if e.Error.Type == "billing_error" || strings.Contains(e.Error.Type, "authentication") || strings.Contains(e.Error.Type, "credential") {
					msg = e.Error.Type // their messages can name the account or a vault
				}
			}
			run.Status = agentrun.Error
			err = add(session.Error, eval.ErrorBody{Message: msg})
		case "session.usage":
			if e.Usage != nil {
				latest = e.Usage
			}
		}
		if err != nil {
			return eval.Run{}, fmt.Errorf("managed: event %s: %w", e.ID, err)
		}
	}
	if kind == session.KindRun && report >= 0 {
		run.Events[report].Kind = session.Report
	}

	last := s.Usage
	if last.ListCost == nil && latest != nil {
		last = *latest
	}
	u, err := usage(s, last)
	if err != nil {
		return eval.Run{}, err
	}
	if err := add(session.Usage, u); err != nil {
		return eval.Run{}, err
	}
	return run, nil
}

func orEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}

// usage is the session's whole usage as one Usage event, priced at the list
// cost Managed Agents reports, which covers everything the session is billed
// for.
func usage(s Session, u Usage) (session.UsageBody, error) {
	var model struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(s.Agent.Model, &model); err != nil {
		_ = json.Unmarshal(s.Agent.Model, &model.ID)
	}
	b := session.UsageBody{
		Model:        model.ID,
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		CacheRead:    u.CacheRead,
		CacheWrite:   u.CacheCreation.OneHour + u.CacheCreation.FiveMins,
	}
	if u.ListCost != nil {
		if u.ListCost.Currency != "USD" {
			return b, fmt.Errorf("managed: session %s: list cost in %q, not USD", s.ID, u.ListCost.Currency)
		}
		cents, err := strconv.ParseInt(u.ListCost.Amount, 10, 64)
		if err != nil {
			return b, fmt.Errorf("managed: session %s: list cost %q: %w", s.ID, u.ListCost.Amount, err)
		}
		b.CostMicroUSD = cents * 10_000
	}
	return b, nil
}
