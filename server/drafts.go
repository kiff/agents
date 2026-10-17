package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/kiff/agents/drafts"
)

// Drafts claims the key of a taf's confirmed KIFF draft (kiff/agents#91).
type Drafts interface {
	Claim(ctx context.Context, tenant, draft, keyRef string) (agentID string, err error)
}

// claimDraft is tafs cloud asking for a confirmed draft's key to be claimed
// into keyRef: the key goes from KIFF to the agents' secrets, never through
// the caller. It answers the draft's agent, the same on a retry.
func (s *Server) claimDraft(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	var body struct {
		KeyRef string `json:"key_ref"`
	}
	if err := decode(r, &body); err != nil {
		return 0, nil, err
	}
	ref := strings.TrimSpace(body.KeyRef)
	if ref == "" || len(ref) > 256 {
		return 0, nil, bad("key_ref is the reference the taf's definition will name, 1 to 256 bytes")
	}
	agent, err := s.Drafts.Claim(ctx, c.Tenant, r.PathValue("draft_id"), ref)
	switch {
	case errors.Is(err, drafts.ErrOtherRef):
		return 0, nil, conflict("this draft's key was already claimed into another key_ref")
	case errors.Is(err, drafts.ErrTaken):
		return 0, nil, conflict("a key is already stored under that key_ref; name another")
	case errors.Is(err, drafts.ErrNotClaimable):
		return 0, nil, conflict("KIFF can't give this draft's key now: the owner hasn't confirmed it yet, or it was claimed or discarded")
	case errors.Is(err, drafts.ErrExpired):
		return 0, nil, conflict("the draft expired in KIFF; the setup agent must submit it again")
	case err != nil:
		return 0, nil, err
	}
	return http.StatusOK, map[string]string{"agent_id": agent}, nil
}
