package planque

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/schedule"
)

// SignInChecker checks a schedule's hosted-browser connection before a firing
// starts a run (schedule.Service.SignedIn, browser batch 2): planque's POST
// /v1/accounts/{tenant}/grants/{grant}/health, with the agent's own KIFF key,
// sending nothing but the grant. A site planque reads signed_out holds the
// run; signed_in, unknown, or a check planque couldn't make runs it. An
// error (no key, planque down, an answer that can't be read) runs it too,
// in schedule.Service: an outage never holds every taf.
func SignInChecker(agents registry.Agents, secrets registry.Secrets, hc *http.Client, base string) func(ctx context.Context, tenant, agent, grant string) (*schedule.SignInStopped, error) {
	return func(ctx context.Context, tenant, agent, grant string) (*schedule.SignInStopped, error) {
		if base == "" {
			return nil, fmt.Errorf("planque: no planque to check a sign-in with")
		}
		d, err := agents.Get(ctx, tenant, agent)
		if err != nil {
			return nil, fmt.Errorf("planque: sign-in check: the agent: %v", err)
		}
		key, err := secrets.Key(ctx, tenant, d.KeyRef)
		if err != nil {
			return nil, fmt.Errorf("planque: sign-in check: the agent's KIFF key: %v", err)
		}
		u := strings.TrimSuffix(base, "/") + "/v1/accounts/" + url.PathEscape(tenant) + "/grants/" + url.PathEscape(grant) + "/health"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Accept", "application/json")
		res, err := hc.Do(req)
		if err != nil {
			return nil, fmt.Errorf("planque: sign-in check: %v", scrubKey(err, key))
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("planque: sign-in check answered %d", res.StatusCode)
		}
		var out struct {
			Sites []struct {
				Site    string `json:"site"`
				Verdict string `json:"verdict"`
			} `json:"sites"`
		}
		if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&out); err != nil {
			return nil, fmt.Errorf("planque: sign-in check: an answer that can't be read: %v", err)
		}
		// Only the verdict is read: the first site whose saved sign-in stopped working.
		for _, s := range out.Sites {
			if s.Verdict == "signed_out" {
				return &schedule.SignInStopped{Site: s.Site}, nil
			}
		}
		return nil, nil
	}
}

// scrubKey keeps the key out of an error's words.
func scrubKey(err error, key string) error {
	if key == "" || !strings.Contains(err.Error(), key) {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), key, "[key]"))
}
