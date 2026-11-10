package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

// The sweep (-sweep) is the test account's teardown, the workflow's last
// step whatever the steps before it did. A run's cleanup archives the tafs
// it made, and KIFF revokes an archived agent's keys; the account's hourly
// prune then deletes both. What a run never cleaned up stays: a run cut off
// before its cleanup. The sweep takes those, and nothing else: the tafs this
// job's runs made (-made, written as each draft is confirmed).
//
// It works as the owner's phone, so it makes only the calls KIFF takes from
// a phone's session: it reads the phone's own access (GET /v1/me/access, the
// account's agents that aren't archived, and which one is Setup's) and
// archives each taf by its id, signed. KIFF never lets a phone's session
// list the account's keys, so the sweep doesn't ask: an earlier version did,
// was refused every time (403), and swept nothing.
//
// A taf this job made that is no longer in the phone's access is already
// archived: nothing to do. Setup's agent is never archived, even if -made
// names it. Tafs left by runs before this one aren't found from a phone's
// session; whoever holds the account's dashboard removes those. The log
// says only how many: the nightly log is public.

// sweepCounts is what a sweep did, for the log.
type sweepCounts struct {
	archived, gone, failed, left int
}

func (c sweepCounts) String() string {
	return fmt.Sprintf("sweep: %d tafs archived (their keys with them), %d already gone, %d failed; %d left to sweep",
		c.archived, c.gone, c.failed, c.left)
}

// runSweep is -sweep: the phone's session in KIFF at apiBase, the tafs
// -made names, and the counts in the log.
func runSweep(ctx context.Context, apiBase string, cfg config, out io.Writer) error {
	if cfg.session == "" || cfg.phone == "" {
		return errors.New("-sweep: E2E_KIFF_SESSION and E2E_PHONE_KEY are required")
	}
	pk, err := parsePhone(cfg.phone)
	if err != nil {
		return err
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	me, err := kiffSession(apiBase, cfg.session, "", pk, hc).do(ctx, http.MethodGet, "/v1/me", nil)
	if err != nil || me.err() != nil {
		return fmt.Errorf("KIFF didn't take the phone's session: %v %v", err, me.err())
	}
	tenant, _ := me.Body["tenant_id"].(string)
	made, err := readMade(cfg.made)
	if err != nil {
		return err
	}
	c, err := sweep(ctx, kiffSession(apiBase, cfg.session, tenant, pk, hc), made)
	fmt.Fprintln(out, c)
	return err
}

// sweep archives the tafs this job's runs made, through KIFF as the owner's
// phone, and counts what is still there after.
func sweep(ctx context.Context, kiffc *client, made []string) (sweepCounts, error) {
	var c sweepCounts
	there, setup, err := phoneAccess(ctx, kiffc)
	if err != nil {
		return c, err
	}
	for _, a := range made {
		switch {
		case a == setup:
			// Setup's agent is the account's, never a run's to take.
		case !there[a]:
			c.gone++
		default:
			ans, err := kiffc.do(ctx, http.MethodPost, "/v1/me/agents/"+url.PathEscape(a)+"/archive", map[string]string{})
			switch {
			case err == nil && ans.err() == nil:
				c.archived++
			case err == nil && ans.Status == http.StatusNotFound:
				// The prune deleted it between the read and the call.
				c.gone++
			default:
				c.failed++
			}
		}
	}
	// What's still there, as KIFF answers the phone after: an archived agent
	// has left its access.
	if there, _, err = phoneAccess(ctx, kiffc); err == nil {
		for _, a := range made {
			if a != setup && there[a] {
				c.left++
			}
		}
	}
	if c.failed > 0 {
		return c, fmt.Errorf("%d of the sweep's calls failed", c.failed)
	}
	return c, err
}

// phoneAccess is the account's agents that aren't archived, as KIFF tells
// the phone (GET /v1/me/access), and which of them is Setup's.
func phoneAccess(ctx context.Context, kiffc *client) (there map[string]bool, setup string, err error) {
	a, err := kiffc.do(ctx, http.MethodGet, "/v1/me/access", nil)
	if err != nil {
		return nil, "", err
	}
	if err := a.err(); err != nil {
		return nil, "", fmt.Errorf("reading the phone's access: %w", err)
	}
	var out struct {
		Agents     map[string]json.RawMessage `json:"agents"`
		SetupAgent string                     `json:"setup_agent"`
	}
	if err := json.Unmarshal([]byte(a.Raw), &out); err != nil {
		return nil, "", fmt.Errorf("reading the phone's access: %w", err)
	}
	there = make(map[string]bool, len(out.Agents))
	for id := range out.Agents {
		there[id] = true
	}
	return there, out.SetupAgent, nil
}

// recordMade appends a taf this run made to -made, for the sweep after it.
func recordMade(path, agent string) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(f, agent)
	return errors.Join(err, f.Close())
}

// readMade is the tafs this job's runs made; none when no run got that far.
func readMade(path string) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) || path == "" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return madeFrom(f)
}

func madeFrom(r io.Reader) ([]string, error) {
	var made []string
	s := bufio.NewScanner(r)
	for s.Scan() {
		if a := strings.TrimSpace(s.Text()); a != "" && !slices.Contains(made, a) {
			made = append(made, a)
		}
	}
	return made, s.Err()
}
