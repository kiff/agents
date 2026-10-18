// Package kiffapi is drafts.KIFF over KIFF's API (api.kiff.dev): a draft
// submitted with the agent's key, its key claimed with the claim secret,
// and who a key speaks for.
package kiffapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kiff/agents/drafts"
)

// Client calls KIFF's API at Base.
type Client struct {
	Base string // https://api.kiff.dev
	HTTP *http.Client
}

var _ drafts.KIFF = (*Client)(nil)

// New returns a client for KIFF's API at base.
func New(base string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{Base: strings.TrimRight(base, "/"), HTTP: hc}
}

// Error is an answer KIFF refused with, and what it said.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("KIFF answered %d: %s", e.Status, e.Message) }

func (c *Client) do(ctx context.Context, method, path, bearer string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("kiffapi: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode >= 300 {
		var e struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Error
		if e.Message != "" {
			msg = e.Message
		}
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return &Error{Status: resp.StatusCode, Message: msg}
	}
	return json.Unmarshal(raw, out)
}

// Submit creates the draft with the agent's key (POST /v1/me/agent-drafts).
func (c *Client) Submit(ctx context.Context, key string, body json.RawMessage) (drafts.Draft, string, error) {
	var out struct {
		drafts.Draft
		ClaimSecret string `json:"claim_secret"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/me/agent-drafts", key, body, &out); err != nil {
		return drafts.Draft{}, "", err
	}
	return out.Draft, out.ClaimSecret, nil
}

// Claim takes the confirmed draft's key with its claim secret
// (POST /v1/agent-drafts/{id}/claim). KIFF's 409 (not confirmed yet, or
// already claimed or discarded) is drafts.ErrNotClaimable, its 410
// drafts.ErrExpired.
func (c *Client) Claim(ctx context.Context, draft, secret string) (drafts.Claimed, error) {
	var out struct {
		AgentID   string `json:"agent_id"`
		KeyID     string `json:"key_id"`
		Plaintext string `json:"plaintext"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/agent-drafts/"+url.PathEscape(draft)+"/claim", secret, []byte("{}"), &out)
	var e *Error
	if errors.As(err, &e) && e.Status == http.StatusConflict {
		return drafts.Claimed{}, fmt.Errorf("%w: %s", drafts.ErrNotClaimable, e.Message)
	}
	if errors.As(err, &e) && e.Status == http.StatusGone {
		return drafts.Claimed{}, drafts.ErrExpired
	}
	if err != nil {
		return drafts.Claimed{}, err
	}
	if out.Plaintext == "" || out.AgentID == "" {
		return drafts.Claimed{}, errors.New("kiffapi: KIFF's claim answer has no key or agent")
	}
	return drafts.Claimed{AgentID: out.AgentID, Key: out.Plaintext, KeyID: out.KeyID}, nil
}

// Me says who key speaks for (GET /v1/me).
func (c *Client) Me(ctx context.Context, key string) (drafts.Me, error) {
	var me drafts.Me
	err := c.do(ctx, http.MethodGet, "/v1/me", key, nil, &me)
	return me, err
}
