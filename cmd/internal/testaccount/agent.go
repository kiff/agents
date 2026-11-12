package testaccount

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// A fresh agent for one run (the Eval's tickets case): an agent of its own on
// the test account, so its Cards' counts start at zero, connected to a vendor
// by an API-key grant that Setup's key asks for and the phone gives the key
// to, with Cards the phone confirms, and its own KIFF key. Remove takes it all
// back: the grant disconnected and the agent archived (its keys with it).

// RunID is a short random id for one run's agent and the names of its tools,
// so nothing an earlier run left can hold them.
func RunID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// AgentSpec is what a fresh agent is made of.
type AgentSpec struct {
	// ID is the agent's id: the run's own (RunID makes one).
	ID    string
	Label string
	// Tools and API are the grant's ask, as planque takes it (kind api_key).
	Tools []map[string]any
	API   map[string]any
	// Cards are the draft's Cards, each without its subject: the agent's id
	// is filled in.
	Cards []map[string]any
}

// Agent is a fresh agent as made.
type Agent struct {
	ID    string
	Key   string
	Grant string
}

// Account is the test account as Setup and the owner's phone reach it.
type Account struct {
	Phone *Phone  // toward planque
	KIFF  *Client // KIFF's API as the phone's session
	Setup *Client // planque as Setup's agent (its key)
}

// SetupClient is planque as Setup's agent, by its key.
func SetupClient(planqueURL, setupKey string, hc *http.Client) *Client {
	return &Client{Base: strings.TrimRight(planqueURL, "/"), HTTP: hc, Auth: func(req *http.Request, _ []byte) {
		req.Header.Set("Authorization", "Bearer "+setupKey)
	}}
}

// NewAgent makes a fresh agent: the grant asked for by Setup with its tools,
// the vendor's key given on the phone (connected), the Cards drafted and
// confirmed by the phone, and the agent's key claimed. made, when set, is
// called with the agent's id as soon as KIFF has it, so a sweep after a run
// cut short still finds it. A step that fails undoes what came before it.
func (a Account) NewAgent(ctx context.Context, spec AgentSpec, made func(agent string) error) (ag Agent, err error) {
	if spec.ID == "" {
		return ag, errors.New("a fresh agent needs its id")
	}
	ag.ID = spec.ID
	defer func() {
		if err != nil {
			err = errors.Join(err, a.Remove(context.WithoutCancel(ctx), ag))
			ag = Agent{}
		}
	}()

	ask := map[string]any{"kind": "api_key", "agent": ag.ID, "agent_name": spec.Label, "tools": spec.Tools, "api": spec.API}
	g, err := a.Setup.Do(ctx, http.MethodPost, a.Phone.AccountPath()+"/grants", ask)
	if err != nil {
		return ag, fmt.Errorf("asking for the grant: %w", err)
	}
	if err := g.Err(); err != nil {
		return ag, fmt.Errorf("asking for the grant: %w", err)
	}
	ag.Grant, _ = g.Body["id"].(string)
	digest, _ := g.Body["digest"].(string)
	if ag.Grant == "" || digest == "" {
		return ag, errors.New("planque's answer to the grant names no id or digest")
	}
	if status, err := a.Phone.GiveKey(ctx, Grant{ID: ag.Grant, Digest: digest}); err != nil {
		return ag, err
	} else if !strings.Contains(status, "connected") {
		return ag, fmt.Errorf("the grant isn't connected after the key: %s", status)
	}

	cards := make([]any, len(spec.Cards))
	for i, c := range spec.Cards {
		card := map[string]any{"subject": ag.ID}
		for k, v := range c {
			card[k] = v
		}
		cards[i] = card
	}
	d, err := a.KIFF.Do(ctx, http.MethodPost, "/v1/me/agent-drafts", map[string]any{"agent_id": ag.ID, "label": spec.Label, "cards": cards})
	if err != nil {
		return ag, fmt.Errorf("drafting the agent: %w", err)
	}
	if err := d.Err(); err != nil {
		return ag, fmt.Errorf("drafting the agent: %w", err)
	}
	draft, _ := d.Body["id"].(string)
	secret, _ := d.Body["claim_secret"].(string)
	if made != nil {
		if err := made(ag.ID); err != nil {
			return ag, fmt.Errorf("recording the agent for the sweep: %w", err)
		}
	}
	if st, _ := d.Body["status"].(string); st != "confirmed" {
		r, err := a.KIFF.Do(ctx, http.MethodGet, "/v1/me/agent-drafts/"+url.PathEscape(draft), nil)
		if err != nil || r.Err() != nil {
			return ag, fmt.Errorf("reading the draft: %v %v", err, r.Err())
		}
		version, _ := r.Body["version"].(string)
		c, err := a.KIFF.Do(ctx, http.MethodPost, "/v1/me/agent-drafts/"+url.PathEscape(draft)+"/confirm", map[string]string{"version": version})
		if err != nil || c.Err() != nil {
			return ag, fmt.Errorf("confirming the draft: %v %v", err, c.Err())
		}
	}
	claim := &Client{Base: a.KIFF.Base, HTTP: a.KIFF.HTTP, Auth: func(req *http.Request, _ []byte) { req.Header.Set("Authorization", "Bearer "+secret) }}
	k, err := claim.Do(ctx, http.MethodPost, "/v1/agent-drafts/"+url.PathEscape(draft)+"/claim", map[string]any{})
	if err != nil || k.Err() != nil {
		return ag, fmt.Errorf("claiming the agent's key: %v %v", err, k.Err())
	}
	if ag.Key, _ = k.Body["plaintext"].(string); ag.Key == "" {
		return ag, errors.New("KIFF's claim gave no key")
	}
	return ag, nil
}

// Remove takes a fresh agent back: its grant disconnected on the phone, and
// the agent archived, which takes its keys with it. Each step runs whatever
// the other did; an agent or grant already gone is fine.
func (a Account) Remove(ctx context.Context, ag Agent) error {
	var errs []error
	if ag.Grant != "" {
		if err := a.Phone.Disconnect(ctx, ag.Grant); err != nil && !strings.HasPrefix(err.Error(), "404") {
			errs = append(errs, fmt.Errorf("disconnecting the grant: %w", err))
		}
	}
	if ag.ID != "" {
		r, err := a.KIFF.Do(ctx, http.MethodPost, "/v1/me/agents/"+url.PathEscape(ag.ID)+"/archive", map[string]string{})
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("archiving the agent: %w", err))
		case r.Status == http.StatusNotFound:
		case r.Err() != nil:
			errs = append(errs, fmt.Errorf("archiving the agent: %w", r.Err()))
		}
	}
	return errors.Join(errs...)
}
