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
	"github.com/kiff/agents/run"
)

// APIURL is KIFF's API.
const APIURL = "https://api.kiff.dev"

// ErrNoHold means KIFF has no such held call for this agent: it never held
// one by that exception, or it is another agent's.
var ErrNoHold = errors.New("kiff: no such held call")

// Withdrawal is where a held call stands after asking KIFF to withdraw it.
type Withdrawal struct {
	// Withdrawn is true when KIFF withdrew it, now or on an earlier ask:
	// the hold stops waiting, and nothing will be sent.
	Withdrawn bool `json:"withdrawn"`
	// Status is KIFF's status for the hold. When it couldn't be withdrawn
	// (already sent, refused or expired), it says which, so the run can
	// record what KIFF did.
	Status string `json:"status"`
}

// Withdraw asks KIFF to withdraw one of the agent's held calls, by its
// exception, because the run that made it was cancelled: POST
// /v1/me/holds/{exception}/withdraw with the agent's own key. Asking again
// answers the same. The key is sent only as the Authorization header and
// never appears in an error.
func Withdraw(ctx context.Context, hc *http.Client, apiURL, key, exception string) (Withdrawal, error) {
	if exception == "" {
		return Withdrawal{}, errors.New("kiff: withdraw needs the hold's exception")
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(apiURL, "/")+"/v1/me/holds/"+url.PathEscape(exception)+"/withdraw", nil)
	if err != nil {
		return Withdrawal{}, fmt.Errorf("kiff: withdraw: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := hc.Do(req)
	if err != nil {
		return Withdrawal{}, fmt.Errorf("kiff: withdraw %s: %w", exception, scrub(err, key))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return Withdrawal{}, fmt.Errorf("kiff: withdraw %s: %w", exception, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var w Withdrawal
		if err := json.Unmarshal(body, &w); err != nil {
			return Withdrawal{}, fmt.Errorf("kiff: withdraw %s: %w", exception, err)
		}
		w.Withdrawn = true
		return w, nil
	case http.StatusConflict:
		// Only KIFF's already_answered, with the hold's status, says what
		// became of the call; any other conflict leaves it unknown.
		var c struct {
			Error string     `json:"error"`
			Hold  Withdrawal `json:"hold"`
		}
		if err := json.Unmarshal(body, &c); err != nil {
			return Withdrawal{}, fmt.Errorf("kiff: withdraw %s: %w", exception, err)
		}
		if c.Error != "already_answered" || c.Hold.Status == "" {
			return Withdrawal{}, fmt.Errorf("kiff: withdraw %s: KIFF answered 409 %q", exception, c.Error)
		}
		c.Hold.Withdrawn = false
		return c.Hold, nil
	case http.StatusNotFound:
		return Withdrawal{}, fmt.Errorf("%w: %s", ErrNoHold, exception)
	}
	return Withdrawal{}, fmt.Errorf("kiff: withdraw %s: KIFF answered %d", exception, resp.StatusCode)
}

// Withdrawer is server.Server's Withdraw: it withdraws a run's held call
// with the KIFF key of the run's own agent, and returns what became of the
// call ("invalidated" when withdrawn, else KIFF's status for it). A hold
// KIFF doesn't know for this key (ErrNoHold: purged, or the agent's key now
// names another KIFF agent) is "" with no error: there is nothing to take
// back. A key that can't be read is an error, since the hold may be live.
func Withdrawer(agents registry.Agents, secrets registry.Secrets, hc *http.Client, apiURL string) func(ctx context.Context, r run.Run, exception string) (string, error) {
	return func(ctx context.Context, r run.Run, exception string) (string, error) {
		d, err := agents.Get(ctx, r.Tenant, r.Agent)
		if err != nil {
			return "", fmt.Errorf("kiff: withdraw: the run's agent: %v", err)
		}
		key, err := secrets.Key(ctx, r.Tenant, d.KeyRef)
		if err != nil {
			return "", fmt.Errorf("kiff: withdraw: the agent's KIFF key: %v", err)
		}
		w, err := Withdraw(ctx, hc, apiURL, key, exception)
		switch {
		case errors.Is(err, ErrNoHold):
			return "", nil // nothing held for this key: nothing to withdraw
		case err != nil:
			return "", err
		case w.Withdrawn && w.Status == "":
			return "invalidated", nil
		}
		return w.Status, nil
	}
}
