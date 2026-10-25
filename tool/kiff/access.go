package kiff

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/run"
)

// AccessTTL is how long an answer is kept per agent and person: a removed
// grant stops a run within a turn or this long, whichever comes later.
const AccessTTL = time.Minute

// AskAccess asks KIFF what the person with this subject may do with the
// agent the key is its own (#221): POST /v1/me/access with the agent's own
// key. The key is sent only as the Authorization header and never appears
// in an error.
func AskAccess(ctx context.Context, hc *http.Client, apiURL, key, subject string) (run.Access, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	body, err := json.Marshal(map[string]string{"subject": subject})
	if err != nil {
		return run.Access{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(apiURL, "/")+"/v1/me/access", bytes.NewReader(body))
	if err != nil {
		return run.Access{}, fmt.Errorf("kiff: access: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return run.Access{}, fmt.Errorf("kiff: access: %w", scrub(err, key))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return run.Access{}, fmt.Errorf("kiff: access: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return run.Access{}, fmt.Errorf("kiff: access: KIFF answered %d", resp.StatusCode)
	}
	var a struct {
		Use     bool `json:"use"`
		Oversee bool `json:"oversee"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return run.Access{}, fmt.Errorf("kiff: access: %w", err)
	}
	return run.Access{Use: a.Use, Oversee: a.Oversee}, nil
}

// Accessor is the runtime's run.AccessFunc: it asks KIFF with the agent's
// own key, and keeps each answer AccessTTL per agent and person. An
// answer that couldn't be had isn't kept, and is no access.
func Accessor(agents registry.Agents, secrets registry.Secrets, hc *http.Client, apiURL string, now func() time.Time) run.AccessFunc {
	if now == nil {
		now = time.Now
	}
	type kept struct {
		a     run.Access
		until time.Time
	}
	var mu sync.Mutex
	cache := map[[3]string]kept{}
	return func(ctx context.Context, tenant, agent, subject string) (run.Access, error) {
		k := [3]string{tenant, agent, subject}
		mu.Lock()
		if c, ok := cache[k]; ok && now().Before(c.until) {
			mu.Unlock()
			return c.a, nil
		}
		mu.Unlock()
		d, err := agents.Get(ctx, tenant, agent)
		if err != nil {
			return run.Access{}, fmt.Errorf("kiff: access: the agent: %v", err)
		}
		key, err := secrets.Key(ctx, tenant, d.KeyRef)
		if err != nil {
			return run.Access{}, fmt.Errorf("kiff: access: the agent's KIFF key: %v", err)
		}
		a, err := AskAccess(ctx, hc, apiURL, key, subject)
		if err != nil {
			return run.Access{}, err
		}
		mu.Lock()
		// Expired answers go as new ones come, so the cache holds about
		// one minute's worth however long the process lives.
		for old, c := range cache {
			if !now().Before(c.until) {
				delete(cache, old)
			}
		}
		cache[k] = kept{a: a, until: now().Add(AccessTTL)}
		mu.Unlock()
		return a, nil
	}
}
