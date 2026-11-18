package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// liveWeeklyChat is Coach's chat on tafs cloud, as the owner's phone uses it
// (docs/api.md, docs/chat.md): a new chat with the owner's request, the chat
// read until Coach is done, and its cards answered as data.
type liveWeeklyChat struct {
	t     *test
	coach string    // Coach's taf id on tafs cloud (GET /v1/setup)
	since time.Time // the run's start: only Coach's runs after it count to the cap
	// usdPerCredit turns tafs cloud's credits into dollars, for the cap.
	usdPerCredit float64
}

func (c *liveWeeklyChat) cloud() *client {
	return &client{Base: strings.TrimRight(c.t.cfg.cloud, "/"), HTTP: c.t.hc, Auth: bearerAuth(c.t.cfg.session)}
}

// coachOf is the account's Coach on tafs cloud.
func coachOf(ctx context.Context, cl *client) (string, error) {
	a, err := cl.Do(ctx, http.MethodGet, "/v1/setup", nil)
	if err != nil {
		return "", err
	}
	if err := a.Err(); err != nil {
		return "", fmt.Errorf("tafs cloud's setup: %w", err)
	}
	var out struct {
		Setup *struct {
			ID string `json:"id"`
		} `json:"setup"`
	}
	if err := json.Unmarshal([]byte(a.Raw), &out); err != nil || out.Setup == nil || out.Setup.ID == "" {
		return "", errors.New("the account has no Coach on tafs cloud")
	}
	return out.Setup.ID, nil
}

func (c *liveWeeklyChat) messages() string {
	return "/v1/tafs/" + url.PathEscape(c.coach) + "/messages"
}

func (c *liveWeeklyChat) Start(ctx context.Context, text string) (string, error) {
	a, err := c.cloud().Do(ctx, http.MethodPost, c.messages(), map[string]any{"text": text, "new_chat": true})
	if err != nil {
		return "", err
	}
	if err := a.Err(); err != nil {
		return "", err
	}
	s, _ := a.Body["session"].(string)
	if s == "" {
		return "", errors.New("tafs cloud started no chat")
	}
	c.t.out.hide(s, "<the chat>")
	return s, nil
}

func (c *liveWeeklyChat) Send(ctx context.Context, session, text string) error {
	a, err := c.cloud().Do(ctx, http.MethodPost, c.messages(), map[string]any{"text": text, "session": session})
	if err != nil {
		return err
	}
	return a.Err()
}

// Thread reads the chat with the phone's signed read of its grants, so
// tafs cloud marks each approval card with what its tap signs (signs).
func (c *liveWeeklyChat) Thread(ctx context.Context, session string) (weeklyThread, error) {
	proof, err := c.t.phone.GrantsProof(ctx)
	if err != nil {
		return weeklyThread{}, err
	}
	path := "/v1/tafs/" + url.PathEscape(c.coach) + "/conversation/" + url.PathEscape(session)
	a, err := c.cloud().DoWith(ctx, http.MethodGet, path, nil, http.Header{"Planque-Device-Signature": {proof}})
	if err != nil {
		return weeklyThread{}, err
	}
	if err := a.Err(); err != nil {
		return weeklyThread{}, err
	}
	return threadOf([]byte(a.Raw))
}

// threadOf reads tafs cloud's thread: whether Coach is at work, and its cards,
// each with whether it's answered and, on an approval, what it showed.
func threadOf(raw []byte) (weeklyThread, error) {
	var w struct {
		Working    *string `json:"working"`
		Components []struct {
			Session   string          `json:"session"`
			Component json.RawMessage `json:"component"`
			Answer    json.RawMessage `json:"answer"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return weeklyThread{}, fmt.Errorf("tafs cloud's thread: %w", err)
	}
	th := weeklyThread{Working: w.Working != nil && *w.Working != ""}
	for _, s := range w.Components {
		var c weeklyThreadCard
		c.Session = s.Session
		if err := json.Unmarshal(s.Component, &c.Card); err != nil {
			continue
		}
		var extra struct {
			Tools []string `json:"tools"`
			Signs *struct {
				Card bool `json:"card"`
			} `json:"signs"`
		}
		_ = json.Unmarshal(s.Component, &extra)
		c.Tools = extra.Tools
		if extra.Signs != nil {
			card := extra.Signs.Card
			c.Signs = &card
		}
		c.Answered = len(s.Answer) > 0 && string(s.Answer) != "null"
		th.Cards = append(th.Cards, c)
	}
	return th, nil
}

func (c *liveWeeklyChat) Answer(ctx context.Context, component string, answer json.RawMessage, session string) error {
	path := "/v1/tafs/" + url.PathEscape(c.coach) + "/components/" + url.PathEscape(component) + "/answer"
	a, err := c.cloud().Do(ctx, http.MethodPost, path, map[string]any{"answer": answer, "session": session})
	if err != nil {
		return err
	}
	return a.Err()
}

// Spent is Coach's credits since the run started, in millionths of a dollar.
func (c *liveWeeklyChat) Spent(ctx context.Context) (int64, error) {
	a, err := c.cloud().Do(ctx, http.MethodGet, "/v1/usage/credits/tafs/"+url.PathEscape(c.coach), nil)
	if err != nil {
		return 0, err
	}
	if err := a.Err(); err != nil {
		return 0, err
	}
	return spentSince([]byte(a.Raw), c.since, c.usdPerCredit)
}

// spentSince sums the credits of Coach's runs that ended since the run
// started, in millionths of a dollar.
func spentSince(raw []byte, since time.Time, usdPerCredit float64) (int64, error) {
	var out struct {
		Items []struct {
			At      time.Time `json:"at"`
			Credits float64   `json:"credits"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, fmt.Errorf("tafs cloud's credits: %w", err)
	}
	var credits float64
	for _, it := range out.Items {
		if !it.At.Before(since) {
			credits += it.Credits
		}
	}
	return int64(credits * usdPerCredit * 1e6), nil
}
