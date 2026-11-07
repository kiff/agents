package kiff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kiff/agents/registry"
)

// AllowanceKept is how long an allowed answer is kept per agent, so a run
// doesn't wait on KIFF each time. A refusal isn't kept: a raise or a pack
// lets the next run start at once. For up to this long after the balance
// is used, a run can still start.
const AllowanceKept = time.Minute

// Allowance asks KIFF whether a new run of the key's agent may start: GET
// /v1/me/credits/allowance with the agent's own key. It answers whether,
// and why not: KIFF's code (credits_used_up, limit_reached or no_plan). A
// KIFF without the route (404) allows. Any other answer is an error, and the
// key never appears in one.
func Allowance(ctx context.Context, hc *http.Client, apiURL, key string) (allowed bool, why string, err error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(apiURL, "/")+"/v1/me/credits/allowance", nil)
	if err != nil {
		return false, "", fmt.Errorf("kiff: allowance: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := hc.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("kiff: allowance: %w", scrub(err, key))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if resp.StatusCode == http.StatusNotFound {
		// A KIFF from before credits has no such route: every run may
		// start, so the order the two deploy in can't stop every taf.
		return true, "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, "", fmt.Errorf("kiff: allowance: %s: %s", resp.Status, strings.TrimSpace(scrub(errors.New(string(b)), key).Error()))
	}
	var out struct {
		Allowed *bool  `json:"allowed"`
		Why     string `json:"why"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Allowed == nil || (!*out.Allowed && out.Why == "") {
		return false, "", errors.New("kiff: allowance: not an answer KIFF gives")
	}
	return *out.Allowed, out.Why, nil
}

// AllowanceChecker is the runtime's runner.Runner.Allowance: it asks KIFF
// with the agent's own key, and keeps an allowed answer for AllowanceKept
// per agent. now is the clock; nil, the system's.
func AllowanceChecker(agents registry.Agents, secrets registry.Secrets, hc *http.Client, apiURL string, now func() time.Time) func(ctx context.Context, tenant, agent string) (bool, string, error) {
	if now == nil {
		now = time.Now
	}
	var (
		mu      sync.Mutex
		allowed = map[string]time.Time{} // tenant|agent → until when an allowed answer holds
	)
	return func(ctx context.Context, tenant, agent string) (bool, string, error) {
		k := tenant + "|" + agent
		mu.Lock()
		until, ok := allowed[k]
		if ok && now().Before(until) {
			mu.Unlock()
			return true, "", nil
		}
		delete(allowed, k)
		mu.Unlock()
		d, err := agents.Get(ctx, tenant, agent)
		if err != nil {
			return false, "", fmt.Errorf("kiff: allowance: the agent: %v", err)
		}
		key, err := secrets.Key(ctx, tenant, d.KeyRef)
		if err != nil {
			return false, "", fmt.Errorf("kiff: allowance: the agent's KIFF key: %v", err)
		}
		ok, why, err := Allowance(ctx, hc, apiURL, key)
		if err != nil || !ok {
			return ok, why, err
		}
		mu.Lock()
		allowed[k] = now().Add(AllowanceKept)
		mu.Unlock()
		return true, "", nil
	}
}
