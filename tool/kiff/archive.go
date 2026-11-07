package kiff

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/kiff/agents/registry"
)

// ArchiveSelf archives the key's own agent in KIFF: POST
// /v1/agents/self/archive with the agent's own key, its last act with it,
// since archiving revokes the agent's keys in KIFF, this one included. A
// 401 or 403 (the key already revoked: an earlier try got through) and a
// 404 (a KIFF without the route, or no such agent there) are done: nothing
// more to take back with this key. Any other answer is an error, and the
// key never appears in one.
func ArchiveSelf(ctx context.Context, hc *http.Client, apiURL, key string) error {
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(apiURL, "/")+"/v1/agents/self/archive", nil)
	if err != nil {
		return fmt.Errorf("kiff: archive: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("kiff: archive: %w", scrub(err, key))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse))
	switch resp.StatusCode {
	case http.StatusOK, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return nil
	}
	return fmt.Errorf("kiff: archive: KIFF answered %d", resp.StatusCode)
}

// Archiver is server.Server's ArchiveKIFF: it archives the agent's KIFF
// agent with the agent's own KIFF key, as the agent is deleted. An agent
// whose key is gone has nothing left to archive with: done.
func Archiver(agents registry.Agents, secrets registry.Secrets, hc *http.Client, apiURL string) func(ctx context.Context, tenant, agent string) error {
	return func(ctx context.Context, tenant, agent string) error {
		d, err := agents.Get(ctx, tenant, agent)
		if err != nil {
			return fmt.Errorf("kiff: archive: the agent: %v", err)
		}
		if d.KeyRef == "" {
			return nil
		}
		key, err := secrets.Key(ctx, tenant, d.KeyRef)
		if err != nil {
			return fmt.Errorf("kiff: archive: the agent's KIFF key: %v", err)
		}
		return ArchiveSelf(ctx, hc, apiURL, key)
	}
}
