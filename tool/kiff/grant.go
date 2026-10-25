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
)

// ResolveGrant asks KIFF who a run grant was signed for (POST
// /v1/me/run-grants/resolve), with the agent's own key: KIFF answers only
// for a grant of that agent. The key and the grant are sent only in the
// request and never appear in an error.
func ResolveGrant(ctx context.Context, hc *http.Client, apiURL, key, grant string) (string, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	body, err := json.Marshal(map[string]string{"grant": grant})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(apiURL, "/")+"/v1/me/run-grants/resolve", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("kiff: run grant: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("kiff: run grant: %w", scrub(scrub(err, key), grant))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return "", fmt.Errorf("kiff: run grant: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("kiff: run grant: KIFF answered %d", resp.StatusCode)
	}
	var a struct {
		Subject string `json:"subject"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("kiff: run grant: %w", err)
	}
	if a.Subject == "" {
		return "", fmt.Errorf("kiff: run grant: KIFF named no person")
	}
	return a.Subject, nil
}

// GrantResolver is the server's ResolveGrant: it asks KIFF with the
// agent's own key. Nothing is kept: a grant is resolved once, when its run
// starts.
func GrantResolver(agents registry.Agents, secrets registry.Secrets, hc *http.Client, apiURL string) func(ctx context.Context, tenant, agent, grant string) (string, error) {
	return func(ctx context.Context, tenant, agent, grant string) (string, error) {
		d, err := agents.Get(ctx, tenant, agent)
		if err != nil {
			return "", fmt.Errorf("kiff: run grant: the agent: %v", err)
		}
		key, err := secrets.Key(ctx, tenant, d.KeyRef)
		if err != nil {
			return "", fmt.Errorf("kiff: run grant: the agent's KIFF key: %v", err)
		}
		return ResolveGrant(ctx, hc, apiURL, key, grant)
	}
}
