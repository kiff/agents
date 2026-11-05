package kiff

import (
	"context"
	"errors"
	"fmt"
)

// Pending is one of the agent's open calls, as the gateway's kiff_pending
// lists it: held (with the owner's answer), sending or unknown.
type Pending struct {
	Tool        string `json:"tool"`
	State       string `json:"state"`
	Answer      string `json:"answer"`
	ExceptionID string `json:"exception_id"`
}

// Pending lists the agent's open calls through the gateway's read-only
// kiff_pending tool, every page of it (as many as Answers reads): nothing
// is decided, drawn or sent. It returns what it read so far with an error when a page fails.
func (c *Client) Pending(ctx context.Context) ([]Pending, error) {
	var out []Pending
	cursor := ""
	for range maxPendingPages {
		args := map[string]any{}
		if cursor != "" {
			args["cursor"] = cursor
		}
		var res struct {
			IsError    bool `json:"isError"`
			Structured struct {
				Calls      []Pending `json:"calls"`
				NextCursor string    `json:"next_cursor"`
			} `json:"structuredContent"`
		}
		if err := c.call(ctx, "tools/call", map[string]any{"name": "kiff_pending", "arguments": args}, &res); err != nil {
			return out, err
		}
		if res.IsError {
			return out, errors.New("kiff: kiff_pending answered with an error")
		}
		out = append(out, res.Structured.Calls...)
		if res.Structured.NextCursor == "" {
			return out, nil
		}
		cursor = res.Structured.NextCursor
	}
	return out, fmt.Errorf("kiff: kiff_pending: more than %d pages", maxPendingPages)
}
