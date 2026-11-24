package kiff

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/tool"
)

// A step the person paused in their own Chrome (tafs docs/chrome.md, the
// floating bar's Pause) waits for them instead of ending the run's turn.
// planque answers the step with a pause and names its run; while that run is
// paused, the call asks planque for the run's state, at planque's address from
// the runtime's configuration and with the taf's own key, never through KIFF's
// gateway, so the person's Record holds the steps alone. On Resume the step is
// sent once more, as a new operation (the paused one never ran); paused again,
// it waits again. Stopped, gone, past PauseWait, or anything planque can't
// say: the pause goes to the model as before.

// PauseWait is how long a call waits for the person to resume a paused run:
// within planque's own hold of a paused run (15 minutes).
var PauseWait = 10 * time.Minute

// PauseEvery is how often a paused run's state is read.
var PauseEvery = 10 * time.Second

// pauseMargin is the run's time kept after a wait, for the model to answer.
const pauseMargin = time.Minute

// pausedCodes are planque's answers for a step in a paused run.
var pausedCodes = map[string]bool{"paused_by_owner": true, "paused_failures": true, "paused_no_progress": true}

// pausedRun is what a paused step's answer names: the run and its grant.
type pausedRun struct{ run, grant string }

// grantID is planque's grant id, the only thing put in the read's path.
var grantID = regexp.MustCompile(`^grt_[A-Za-z0-9_-]{1,64}$`)

// pausedOf reads a refused answer's structured content: a pause with its run
// and grant, or nothing.
func pausedOf(structured json.RawMessage) pausedRun {
	var sc struct {
		RefusedBy string `json:"refused_by"`
		Code      string `json:"code"`
		Run       string `json:"run"`
		Grant     string `json:"grant"`
	}
	if json.Unmarshal(structured, &sc) != nil || sc.RefusedBy != "connection" || !pausedCodes[sc.Code] ||
		sc.Run == "" || len(sc.Run) > 200 || !grantID.MatchString(sc.Grant) {
		return pausedRun{}
	}
	return pausedRun{run: sc.Run, grant: sc.Grant}
}

// WithPlanque lets the client's calls wait on a run paused in the owner's
// Chrome, reading its state at base for tenant. Empty, they don't.
func (c *Client) WithPlanque(base, tenant string) *Client {
	c.planque, c.tenant = strings.TrimRight(base, "/"), tenant
	return c
}

// waitPaused is the call's answer once the person resumed the run its step
// was paused in, or out as it came.
func (t *gatewayTool) waitPaused(ctx context.Context, input json.RawMessage, out tool.Result, p pausedRun) tool.Result {
	if p.run == "" || t.c.planque == "" || t.c.tenant == "" || !out.IsError {
		return out
	}
	deadline := time.Now().Add(PauseWait)
	if d, ok := ctx.Deadline(); ok && d.Add(-pauseMargin).Before(deadline) {
		deadline = d.Add(-pauseMargin)
	}
	resent := 0
	for time.Now().Add(PauseEvery).Before(deadline) {
		select {
		case <-ctx.Done():
			return out
		case <-time.After(PauseEvery):
		}
		at, state, err := t.c.runState(ctx, p.grant)
		if err != nil || at != p.run || state != "paused" && state != "working" {
			return out
		}
		if state == "paused" {
			continue
		}
		// Resumed: the step once more, as its own operation. Paused again, it waits again.
		resent++
		again, m, err := t.callMeta(tool.WithCall(ctx, tool.CallFrom(ctx)+"\x00resumed\x00"+strconv.Itoa(resent)), withoutOperation(input))
		if err != nil {
			return out
		}
		if m.paused.run != p.run {
			return again
		}
		out = again
	}
	return out
}

// runState is planque's answer for the grant's run in the owner's Chrome: its
// id and its state (working, paused, stopped, ended).
func (c *Client) runState(ctx context.Context, grant string) (string, string, error) {
	u := c.planque + "/v1/accounts/" + url.PathEscape(c.tenant) + "/grants/" + url.PathEscape(grant) + "/chrome/run"
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", scrub(err, c.key)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("planque answered %d", resp.StatusCode)
	}
	var out struct {
		Run   string `json:"run"`
		State string `json:"state"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 4<<10)).Decode(&out); err != nil {
		return "", "", err
	}
	return out.Run, out.State, nil
}

// withoutOperation is input without an operation the model named: the step
// sent again after a pause is its own operation, as the paused one never ran.
func withoutOperation(input json.RawMessage) json.RawMessage {
	var args map[string]json.RawMessage
	if json.Unmarshal(input, &args) != nil {
		return input
	}
	if _, ok := args[run.OperationArg]; !ok {
		return input
	}
	delete(args, run.OperationArg)
	b, err := json.Marshal(args)
	if err != nil {
		return input
	}
	return b
}
