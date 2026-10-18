package kiff

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Room is what one of the agent's Card limits has left, as kiff_card says:
// for each action the Card covers, in the limit's own units (a count, or an
// amount in the Card's minor units). Several limits may cover one action;
// each is its own Room.
type Room struct {
	Card      string    // the Card's ID
	Action    string    // a tool the Card covers
	Quantity  string    // "count", "sum(amount)"…
	Window    string    // "calendar_day"…
	Limit     int64     // the window's limit, room added included
	Remaining int64     // what's left of it now
	ResetsAt  time.Time // when the window starts again; zero if KIFF didn't say
}

// CardRoom reads what the agent's Cards have left now, through the gateway's
// read-only kiff_card tool: no call is made against a Card, and nothing is
// drawn. The numbers are as of now; every call is still checked when made.
func (c *Client) CardRoom(ctx context.Context) ([]Room, error) {
	var res struct {
		IsError    bool `json:"isError"`
		Structured struct {
			Cards []struct {
				ID     string `json:"id"`
				Grants []struct {
					Action string `json:"action"`
				} `json:"grants"`
				Limits []struct {
					Quantity  string `json:"quantity"`
					Window    string `json:"window"`
					Limit     int64  `json:"limit"`
					Remaining int64  `json:"remaining"`
					Forecast  struct {
						ResetsAt string `json:"resets_at"`
					} `json:"forecast"`
				} `json:"limits"`
			} `json:"cards"`
		} `json:"structuredContent"`
	}
	if err := c.call(ctx, "tools/call", map[string]any{"name": "kiff_card", "arguments": map[string]any{}}, &res); err != nil {
		return nil, err
	}
	if res.IsError {
		return nil, errors.New("kiff: kiff_card answered with an error")
	}
	var out []Room
	for _, card := range res.Structured.Cards {
		for _, l := range card.Limits {
			var resets time.Time
			if l.Forecast.ResetsAt != "" {
				t, err := time.Parse(time.RFC3339Nano, l.Forecast.ResetsAt)
				if err != nil {
					// No Card id: this error can reach a public log.
					return nil, fmt.Errorf("kiff: kiff_card: a limit's resets_at %q: %w", l.Forecast.ResetsAt, err)
				}
				resets = t.UTC()
			}
			for _, g := range card.Grants {
				out = append(out, Room{Card: card.ID, Action: g.Action, Quantity: l.Quantity, Window: l.Window,
					Limit: l.Limit, Remaining: l.Remaining, ResetsAt: resets})
			}
		}
	}
	return out, nil
}
