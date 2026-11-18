package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// weeklyTafTools reads the tools tafs cloud keeps for a taf (tafs#1367: an
// approved connection's tools are its taf's from then on).
type weeklyTafTools interface {
	TafTools(ctx context.Context, agent string) ([]string, error)
}

// toolsGained is the weekly proof's check after the approval: every tool of
// the approved connection is the taf's, by the grant's own names. It says
// which are missing.
func toolsGained(ctx context.Context, r weeklyTafTools, agent string, connection []string) error {
	have, err := r.TafTools(ctx, agent)
	if err != nil {
		return fmt.Errorf("reading the taf's tools: %w", err)
	}
	var missing []string
	for _, t := range connection {
		if !slices.Contains(have, t) {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the taf didn't gain the connection's %s", strings.Join(missing, ", "))
	}
	return nil
}
