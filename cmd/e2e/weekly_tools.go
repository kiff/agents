package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// weeklyTafTools reads the tools tafs cloud keeps for a taf (tafs#1367: an
// approved connection's tools are its taf's from then on; tafs#1371 shows
// them, by name, to whoever manages the account's Cards).
type weeklyTafTools interface {
	// TafTools is the taf's kept tools, by the KIFF agent it acts as; none
	// when tafs cloud has no such taf yet.
	TafTools(ctx context.Context, agent string) ([]string, error)
}

// toolsKept is the weekly proof's check after the approval, both ways:
// every tool of the approved connection is the taf's now,
// and every tool the taf gained since before the approval is one the signed
// card showed. A tool kept that the owner never saw fails as much as one
// missing.
func toolsKept(before, after, connection, signed []string) error {
	var missing, unseen []string
	for _, t := range connection {
		if !slices.Contains(after, t) {
			missing = append(missing, t)
		}
	}
	for _, t := range after {
		if !slices.Contains(before, t) && !slices.Contains(signed, t) {
			unseen = append(unseen, t)
		}
	}
	switch {
	case len(missing) > 0:
		return fmt.Errorf("the taf didn't gain the connection's %s", strings.Join(missing, ", "))
	case len(unseen) > 0:
		return fmt.Errorf("a tool kept without a Card: %s", strings.Join(unseen, ", "))
	}
	return nil
}

// liveTafTools reads GET /v1/tafs as the owner's phone, and picks the taf by
// the KIFF agent it acts as.
type liveTafTools struct{ c *liveWeeklyChat }

func (l liveTafTools) TafTools(ctx context.Context, agent string) ([]string, error) {
	a, err := l.c.cloud().Do(ctx, http.MethodGet, "/v1/tafs", nil)
	if err != nil {
		return nil, err
	}
	if err := a.Err(); err != nil {
		return nil, err
	}
	return tafToolsOf([]byte(a.Raw), agent)
}

// tafToolsOf is the kept tools of the taf acting as agent in GET /v1/tafs's
// answer; none when it isn't there, or keeps none.
func tafToolsOf(raw []byte, agent string) ([]string, error) {
	var out struct {
		Tafs []struct {
			KIFFAgent string   `json:"kiff_agent"`
			Tools     []string `json:"tools"`
		} `json:"tafs"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("tafs cloud's tafs: %w", err)
	}
	for _, t := range out.Tafs {
		if t.KIFFAgent == agent {
			return t.Tools, nil
		}
	}
	return nil, nil
}
