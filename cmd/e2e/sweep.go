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
// before its cleanup, and the runs before KIFF revoked an archived agent's
// keys, whose agents the prune deleted and whose keys stayed live. The sweep
// takes both, and nothing else:
//   - the tafs this job's runs made (-made, written as each draft is
//     confirmed), whenever they were last used;
//   - any other taf whose keys have all been idle for sweepIdle.
//
// A taf here is an agent whose every active key is one KIFF minted when its
// draft was claimed: bound to it, labelled "draft …", and a taf's gateway
// key only. An agent with any other key (Setup's, a gateway or connector
// agent's) is never touched, nor is a key bound to no agent (the account's).
// Each taf is archived, which revokes its keys; one whose agent is already
// gone has its keys revoked one by one. The log says only how many: the
// nightly log is public.

// sweepIdle is how long a taf's keys must all have been unused before the
// sweep takes a taf this job didn't make.
const sweepIdle = 14 * 24 * time.Hour

// gatewayAgent is the role of the key KIFF mints for a claimed draft.
const gatewayAgent = "gateway_agent"

// listedKey is an active key as GET /v1/keys lists it.
type listedKey struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`
	Roles      []string   `json:"roles"`
	AgentID    string     `json:"agent_id"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

// draftKey is whether KIFF minted k for a claimed taf draft.
func (k listedKey) draftKey() bool {
	return k.AgentID != "" && strings.HasPrefix(k.Label, "draft ") && slices.Equal(k.Roles, []string{gatewayAgent})
}

// idle is whether k has gone unused, or unused since it was made, for sweepIdle.
func (k listedKey) idle(now time.Time) bool {
	last := k.CreatedAt
	if k.LastUsedAt != nil {
		last = *k.LastUsedAt
	}
	return now.Sub(last) > sweepIdle
}

// sweepPlan is the tafs to take, each with its active keys, in the order the
// keys are listed: the ones made (this job's), and any other whose keys are
// all idle.
func sweepPlan(keys []listedKey, made []string, now time.Time) (agents []string, keysOf map[string][]string) {
	byAgent := map[string][]listedKey{}
	var order []string
	for _, k := range keys {
		if k.AgentID == "" {
			continue
		}
		if _, seen := byAgent[k.AgentID]; !seen {
			order = append(order, k.AgentID)
		}
		byAgent[k.AgentID] = append(byAgent[k.AgentID], k)
	}
	keysOf = map[string][]string{}
	for _, a := range order {
		ks := byAgent[a]
		taf, allIdle := true, true
		for _, k := range ks {
			taf = taf && k.draftKey()
			allIdle = allIdle && k.idle(now)
		}
		if !taf || (!allIdle && !slices.Contains(made, a)) {
			continue
		}
		agents = append(agents, a)
		for _, k := range ks {
			keysOf[a] = append(keysOf[a], k.ID)
		}
	}
	return agents, keysOf
}

// sweepCounts is what a sweep did, for the log.
type sweepCounts struct {
	archived, revoked, failed, left int
}

func (c sweepCounts) String() string {
	return fmt.Sprintf("sweep: %d tafs archived (their keys with them), %d keys revoked whose taf was already gone, %d failed; %d left to sweep",
		c.archived, c.revoked, c.failed, c.left)
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
	c, err := sweep(ctx, kiffSession(apiBase, cfg.session, tenant, pk, hc), made, time.Now())
	fmt.Fprintln(out, c)
	return err
}

// sweep takes the tafs sweepPlan names, through KIFF as the owner's phone.
func sweep(ctx context.Context, kiffc *client, made []string, now time.Time) (sweepCounts, error) {
	var c sweepCounts
	keys, err := activeKeys(ctx, kiffc)
	if err != nil {
		return c, err
	}
	agents, keysOf := sweepPlan(keys, made, now)
	for _, a := range agents {
		ans, err := kiffc.do(ctx, http.MethodPost, "/v1/me/agents/"+url.PathEscape(a)+"/archive", map[string]string{})
		switch {
		case err == nil && ans.err() == nil:
			c.archived++
		case err == nil && ans.Status == http.StatusNotFound:
			// The prune deleted the agent, archived before KIFF revoked
			// an archived agent's keys: its keys go one by one.
			for _, k := range keysOf[a] {
				if r, err := kiffc.do(ctx, http.MethodDelete, "/v1/keys/"+url.PathEscape(k), nil); err == nil && (r.err() == nil || r.Status == http.StatusNotFound) {
					c.revoked++
				} else {
					c.failed++
				}
			}
		default:
			c.failed++
		}
	}
	// What's still there, as KIFF lists it after: archiving revokes the keys.
	if keys, err = activeKeys(ctx, kiffc); err == nil {
		left, _ := sweepPlan(keys, made, now)
		c.left = len(left)
	}
	if c.failed > 0 {
		return c, fmt.Errorf("%d of the sweep's calls failed", c.failed)
	}
	return c, nil
}

func activeKeys(ctx context.Context, kiffc *client) ([]listedKey, error) {
	a, err := kiffc.do(ctx, http.MethodGet, "/v1/keys", nil)
	if err != nil {
		return nil, err
	}
	if err := a.err(); err != nil {
		return nil, fmt.Errorf("listing the account's keys: %w", err)
	}
	var out struct {
		Keys []listedKey `json:"keys"`
	}
	if err := json.Unmarshal([]byte(a.Raw), &out); err != nil {
		return nil, fmt.Errorf("listing the account's keys: %w", err)
	}
	return out.Keys, nil
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
