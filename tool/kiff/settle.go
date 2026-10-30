package kiff

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/tool"
)

// Again is the operation ID a call goes out with the second time, once KIFF
// proved its first try had no effect (Settle): fixed from the first, so a
// crash while it goes is still answered with one outcome.
func Again(op string) string { return op + "-again" }

// Settle is res, the outcome of the crashed run's call q repeated by its
// operation ID, once KIFF has tried to settle it when it's unknown (sent, and
// the tool's response lost). KIFF asks the connection's own read-only status
// lookup, never the action: POST …/operations/{key}/reconcile with the
// agent's key. When it proves the first try had no final effect, the call is
// sent once more, as Again. Otherwise it stays unknown, with what KIFF said,
// and nothing is sent: the owner, or a read, checks it. Any other outcome is
// returned as it is.
func Settle(ctx context.Context, a agent.Agent, q run.Requirement, res tool.Result) (tool.Result, error) {
	if res.State != "unknown" || res.Invocation == "" || q.Operation == "" {
		return res, nil
	}
	t := find(a, q.Tool)
	if t == nil {
		return res, nil
	}
	proven, said, err := t.c.reconcile(ctx, res.Invocation)
	if err != nil {
		res.Text += "\n\nKIFF couldn't be asked whether it took effect: " + err.Error()
		return res, nil
	}
	if !proven {
		res.Text += "\n\nKIFF checked: " + said
		return res, nil
	}
	input, err := withOperation(q.Input, Again(q.Operation))
	if err != nil {
		return res, fmt.Errorf("kiff: call %s: %w", q.CallID, err)
	}
	again, m, err := t.callMeta(ctx, input)
	if err != nil {
		return res, fmt.Errorf("kiff: sending call %s again: %w", q.CallID, err)
	}
	if m.Retry == "same_call" || again.Held() {
		return res, fmt.Errorf("%w: call %s, sent again, is %s", ErrNotFinal, q.CallID, m.State)
	}
	if again.State == "" || again.State == "forwarded" {
		again.Text = "KIFF's status lookup proved the first try had no effect, so it was sent once more:\n\n" + again.Text
	}
	return again, nil
}

// reconcile asks KIFF to settle the unknown outcome of the call it keys key.
// proven is true when KIFF proved it had no final effect (200); said is what
// KIFF answered when it couldn't (409, 503). Any other answer is an error.
func (c *Client) reconcile(ctx context.Context, key string) (proven bool, said string, err error) {
	u, err := reconcileURL(c.url, key)
	if err != nil {
		return false, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return false, "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return false, "", scrub(err, c.key)
	}
	defer resp.Body.Close()
	var body struct {
		Message string `json:"message"`
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return false, "", err
	}
	_ = json.Unmarshal(raw, &body)
	switch resp.StatusCode {
	case http.StatusOK:
		return true, body.Message, nil
	case http.StatusConflict, http.StatusServiceUnavailable:
		return false, body.Message, nil
	}
	return false, "", fmt.Errorf("reconcile answered %d", resp.StatusCode)
}

// reconcileURL is where the gateway at mcpURL reconciles the call key: the
// gateway's own /mcp reconciles under /v1, and a link's /a/{id}/mcp under
// /a/{id}.
func reconcileURL(mcpURL, key string) (string, error) {
	u, err := url.Parse(mcpURL)
	if err != nil {
		return "", err
	}
	path := strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), "/mcp")
	if path == "" {
		path = "/v1"
	}
	u.Path, u.RawQuery = path+"/operations/"+key+"/reconcile", ""
	u.RawPath = path + "/operations/" + url.PathEscape(key) + "/reconcile"
	return u.String(), nil
}
