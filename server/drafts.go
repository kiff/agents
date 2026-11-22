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

// DraftReader answers a submitted draft's agent without claiming it. A
// Drafts that is one serves GET /drafts/{draft_id}.
type DraftReader interface {
	Agent(ctx context.Context, tenant, draft string) (agentID string, claimed bool, err error)
}

// DraftLister lists the caller's drafts no taf has claimed. A Drafts that is
// one serves GET /drafts?claimed=false.
type DraftLister interface {
	Unclaimed(ctx context.Context, tenant string) ([]drafts.Unclaimed, error)
}

// listDrafts is tafs cloud asking for the caller's drafts whose key no taf
// has claimed (kiff/tafs#1652): each with its agent, its name and who the
// setup ran for, never a secret or a key. Only claimed=false is served.
func (s *Server) listDrafts(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	if r.URL.Query().Get("claimed") != "false" {
		return 0, nil, bad("claimed=false is the only list served: the drafts no taf has claimed")
	}
	l, ok := s.Drafts.(DraftLister)
	if !ok {
		return 0, nil, drafts.ErrNotFound
	}
	list, err := l.Unclaimed(ctx, c.Tenant)
	if errors.Is(err, drafts.ErrNoList) {
		return 0, nil, drafts.ErrNotFound
	}
	if err != nil {
		return 0, nil, err
	}
	type listed struct {
		DraftID   string            `json:"draft_id"`
		AgentID   string            `json:"agent_id"`
		Name      string            `json:"name,omitempty"`
		Requester *drafts.Requester `json:"requester,omitempty"`
	}
	out := make([]listed, 0, len(list))
	for _, d := range list {
		out = append(out, listed{DraftID: d.ID, AgentID: d.AgentID, Name: d.Label, Requester: d.Requester})
	}
	return http.StatusOK, map[string]any{"drafts": out}, nil
}

// getDraft is tafs cloud asking which agent a draft submitted on the caller's
// tenant names, and whether its key is claimed: a setup's result names a
// draft and an agent, and only this record says they belong together.
func (s *Server) getDraft(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	read, ok := s.Drafts.(DraftReader)
	if !ok {
		return 0, nil, drafts.ErrNotFound
	}
	agent, claimed, err := read.Agent(ctx, c.Tenant, r.PathValue("draft_id"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"agent_id": agent, "claimed": claimed}, nil
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
		return 0, nil, conflict("draft_claimed", "this draft's key was already claimed into another key_ref")
	case errors.Is(err, drafts.ErrTaken):
		return 0, nil, conflict("key_ref_taken", "a key is already stored under that key_ref; name another")
	case errors.Is(err, drafts.ErrNotClaimable):
		return 0, nil, conflict("draft_not_ready", "KIFF can't give this draft's key now: the owner hasn't confirmed it yet, or it was claimed or discarded")
	case errors.Is(err, drafts.ErrExpired):
		return 0, nil, conflict("draft_expired", "the draft expired in KIFF; the setup agent must submit it again")
	case err != nil:
		return 0, nil, err
	}
	return http.StatusOK, map[string]string{"agent_id": agent}, nil
}
