package kiff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/kiff/agents/registry"
)

// trialAnswer is KIFF's account of a trial (kiff/tafs#173): whether it is
// still trying, and its calls' counts, kept after it ends.
type trialAnswer struct {
	Status  string `json:"status"`
	Calls   int    `json:"calls"`
	Held    int    `json:"held"`
	Refused int    `json:"refused"`
}

// EndTrial ends the trial of draft in KIFF with the agent's own key: DELETE
// /v1/me/agent-drafts/{draft}/trial revokes its trial Cards and answers its
// final counts. A trial KIFF had already ended (Install, Go back, its
// expiry) is read instead, so asking again answers the same counts. The key
// is sent only as the Authorization header and never appears in an error.
func EndTrial(ctx context.Context, hc *http.Client, apiURL, key, draft string) (registry.TrialCounts, error) {
	if draft == "" {
		return registry.TrialCounts{}, errors.New("kiff: ending a trial needs its draft")
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	path := strings.TrimSuffix(apiURL, "/") + "/v1/me/agent-drafts/" + url.PathEscape(draft) + "/trial"
	status, a, err := trialCall(ctx, hc, http.MethodDelete, path, key)
	if err == nil && status == http.StatusOK {
		return a.counts(), nil
	}
	if err != nil {
		return registry.TrialCounts{}, fmt.Errorf("kiff: ending the trial of %s: %w", draft, scrub(err, key))
	}
	if status != http.StatusNotFound && status != http.StatusConflict {
		return registry.TrialCounts{}, fmt.Errorf("kiff: ending the trial of %s: KIFF answered %d", draft, status)
	}
	// Not trying any more: read how it ended.
	status, a, err = trialCall(ctx, hc, http.MethodGet, path, key)
	if err != nil {
		return registry.TrialCounts{}, fmt.Errorf("kiff: reading the trial of %s: %w", draft, scrub(err, key))
	}
	if status != http.StatusOK || a.Status != "ended" {
		return registry.TrialCounts{}, fmt.Errorf("kiff: the trial of %s wouldn't end: KIFF answered %d %q", draft, status, a.Status)
	}
	return a.counts(), nil
}

func (a trialAnswer) counts() registry.TrialCounts {
	return registry.TrialCounts{Calls: a.Calls, Held: a.Held, Refused: a.Refused}
}

func trialCall(ctx context.Context, hc *http.Client, method, path, key string) (int, trialAnswer, error) {
	req, err := http.NewRequestWithContext(ctx, method, path, nil)
	if err != nil {
		return 0, trialAnswer{}, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := hc.Do(req)
	if err != nil {
		return 0, trialAnswer{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return 0, trialAnswer{}, err
	}
	var a trialAnswer
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &a); err != nil {
			return 0, trialAnswer{}, err
		}
	}
	return resp.StatusCode, a, nil
}

// TrialEnder is server.Server's EndKIFFTrial: it ends a trial in KIFF with
// the KIFF key of the trial's own agent, which may end only its own trials.
func TrialEnder(agents registry.Agents, secrets registry.Secrets, hc *http.Client, apiURL string) func(ctx context.Context, t registry.Trial) (registry.TrialCounts, error) {
	return func(ctx context.Context, t registry.Trial) (registry.TrialCounts, error) {
		d, err := agents.Get(ctx, t.Tenant, t.Agent)
		if err != nil {
			return registry.TrialCounts{}, fmt.Errorf("kiff: ending a trial: its agent: %v", err)
		}
		key, err := secrets.Key(ctx, t.Tenant, d.KeyRef)
		if err != nil {
			return registry.TrialCounts{}, fmt.Errorf("kiff: ending a trial: the agent's KIFF key: %v", err)
		}
		return EndTrial(ctx, hc, apiURL, key, t.Draft)
	}
}
