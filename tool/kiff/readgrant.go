package kiff

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/run"
)

// ResolveReadGrant asks KIFF what a read grant allows (POST
// /v1/me/read-grants/resolve), with the agent's own key: KIFF answers only
// for a grant of that agent whose reader still oversees it. The key and the
// grant are sent only in the request and never appear in an error.
func ResolveReadGrant(ctx context.Context, hc *http.Client, apiURL, key, grant string) (run.ReadGrant, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	body, err := json.Marshal(map[string]string{"grant": grant})
	if err != nil {
		return run.ReadGrant{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(apiURL, "/")+"/v1/me/read-grants/resolve", bytes.NewReader(body))
	if err != nil {
		return run.ReadGrant{}, fmt.Errorf("kiff: read grant: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return run.ReadGrant{}, fmt.Errorf("kiff: read grant: %w", scrub(scrub(err, key), grant))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return run.ReadGrant{}, fmt.Errorf("kiff: read grant: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return run.ReadGrant{}, fmt.Errorf("kiff: read grant: KIFF answered %d", resp.StatusCode)
	}
	var g struct {
		Reader  string `json:"reader"`
		Owner   string `json:"owner"`
		Session string `json:"session"`
		Purpose string `json:"purpose"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		return run.ReadGrant{}, fmt.Errorf("kiff: read grant: %w", err)
	}
	if g.Reader == "" || g.Owner == "" {
		return run.ReadGrant{}, fmt.Errorf("kiff: read grant: KIFF named no reader or owner")
	}
	return run.ReadGrant{Reader: g.Reader, Owner: g.Owner, Session: g.Session, Purpose: g.Purpose}, nil
}

// ReadGrantResolver is the server's ResolveReadGrant: it asks KIFF with the
// agent's own key, each time a grant is used. Nothing is kept.
func ReadGrantResolver(agents registry.Agents, secrets registry.Secrets, hc *http.Client, apiURL string) run.ReadGrantFunc {
	return func(ctx context.Context, tenant, agent, grant string) (run.ReadGrant, error) {
		d, err := agents.Get(ctx, tenant, agent)
		if err != nil {
			return run.ReadGrant{}, fmt.Errorf("kiff: read grant: the agent: %v", err)
		}
		key, err := secrets.Key(ctx, tenant, d.KeyRef)
		if err != nil {
			return run.ReadGrant{}, fmt.Errorf("kiff: read grant: the agent's KIFF key: %v", err)
		}
		return ResolveReadGrant(ctx, hc, apiURL, key, grant)
	}
}
