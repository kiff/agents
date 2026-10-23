package kiff

import (
	"context"
	"errors"
	"fmt"

	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/run"
)

// maxPendingPages bounds how many pages of kiff_pending Answers reads.
const maxPendingPages = 10

// Answers reads, through the gateway's read-only kiff_pending tool, what
// KIFF answered on each of the agent's held calls named in exceptions, by
// exception: approved, refused, invalidated (the agent's own run withdrew
// it) or expired (the hold ran out, or its Card or an approval was
// withdrawn). A hold still waiting, one KIFF couldn't say for, and one not
// listed are left out. It reads pages until every exception is found or
// none are left. Nothing is collected or sent: collecting stays the run's
// own retry, with the call's operation ID.
func (c *Client) Answers(ctx context.Context, exceptions []string) (map[string]string, error) {
	want := map[string]bool{}
	for _, e := range exceptions {
		if e != "" {
			want[e] = true
		}
	}
	out := map[string]string{}
	cursor := ""
	for page := 0; len(want) > 0 && page < maxPendingPages; page++ {
		args := map[string]any{}
		if cursor != "" {
			args["cursor"] = cursor
		}
		var res struct {
			IsError    bool `json:"isError"`
			Structured struct {
				Calls []struct {
					State       string `json:"state"`
					Answer      string `json:"answer"`
					ExceptionID string `json:"exception_id"`
				} `json:"calls"`
				NextCursor string `json:"next_cursor"`
			} `json:"structuredContent"`
		}
		if err := c.call(ctx, "tools/call", map[string]any{"name": "kiff_pending", "arguments": args}, &res); err != nil {
			return nil, err
		}
		if res.IsError {
			return nil, errors.New("kiff: kiff_pending answered with an error")
		}
		for _, call := range res.Structured.Calls {
			if call.State != "held" || !want[call.ExceptionID] {
				continue
			}
			if a := heldAnswer(call.Answer); a != "" {
				out[call.ExceptionID] = a
			}
			delete(want, call.ExceptionID)
		}
		if res.Structured.NextCursor == "" {
			break
		}
		cursor = res.Structured.NextCursor
	}
	return out, nil
}

// heldAnswer is kiff_pending's answer as a run records it (run.Requirement's
// Answer), or "" while there is none to record.
func heldAnswer(answer string) string {
	switch answer {
	case "approved", "refused":
		return answer
	case "withdrawn":
		return "invalidated"
	case "ended":
		return "expired"
	}
	return "" // waiting, unavailable, or an answer this runtime doesn't know
}

// Answerer reads KIFF's answers on run r's held calls (Answers) through the
// gateway of the run's agent, with that agent's own KIFF key: what a
// continue asks for when no alert has said yet (kiff/tafs#206).
func Answerer(agents registry.Agents, secrets registry.Secrets) func(ctx context.Context, r run.Run) (map[string]string, error) {
	return func(ctx context.Context, r run.Run) (map[string]string, error) {
		d, err := agents.Get(ctx, r.Tenant, r.Agent)
		if err != nil {
			return nil, fmt.Errorf("kiff: answers: the run's agent: %v", err)
		}
		key, err := secrets.Key(ctx, r.Tenant, d.KeyRef)
		if err != nil {
			return nil, fmt.Errorf("kiff: answers: the agent's KIFF key: %v", err)
		}
		var exceptions []string
		for _, q := range r.Requirements {
			exceptions = append(exceptions, q.Exception)
		}
		return New(d.Gateway, key, nil).Answers(ctx, exceptions)
	}
}
