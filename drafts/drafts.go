// Package drafts is a taf's KIFF draft, from the setup agent's submit to
// the claim of its key (kiff/agents#91, agreed on kiff/tafs#50).
//
// The setup agent submits the taf's draft (its agent and Cards) with its own
// KIFF key; KIFF answers with the draft's claim secret, shown once. The
// secret mints the taf's key, so it never reaches the model: it is kept
// sealed under the tenant until the owner's phone has confirmed the draft
// and tafs cloud asks for the claim. The claim takes the key from KIFF,
// checks it is bound to the draft's agent on this tenant, and writes it
// where tafs cloud's definition will name it. The key never passes through
// the model, the phone or tafs cloud.
package drafts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Draft is a draft as KIFF answers it, without its claim secret.
type Draft struct {
	ID      string   `json:"id"`
	AgentID string   `json:"agent_id"`
	Status  string   `json:"status"`
	CardIDs []string `json:"card_ids,omitempty"`
}

// Record is what is kept of one submitted draft: its claim secret until the
// key is claimed, then what the claim did.
type Record struct {
	AgentID string `json:"agent_id"`
	// Secret is the claim secret while the draft is unclaimed; empty after.
	Secret string `json:"secret,omitempty"`
	// KeyRef and Claimed say where the key went, once claimed.
	KeyRef  string `json:"key_ref,omitempty"`
	Claimed bool   `json:"claimed,omitempty"`
}

// Store keeps each submitted draft's Record under its tenant. A Store keeps
// one tenant's records out of another's reach.
type Store interface {
	Put(ctx context.Context, tenant, draft string, r Record) error
	Get(ctx context.Context, tenant, draft string) (Record, error)
}

// Keys holds agents' KIFF keys by reference, and writes one only where
// none is: it never replaces another agent's key.
type Keys interface {
	// Key returns the key stored under ref, if any.
	Key(ctx context.Context, tenant, ref string) (key string, found bool, err error)
	// Create stores key under ref; ErrTaken if one is already there.
	Create(ctx context.Context, tenant, ref, key string) error
}

// Claimed is a key KIFF minted for a confirmed draft.
type Claimed struct {
	AgentID string
	Key     string
}

// Me is who a key speaks for, as KIFF answers it.
type Me struct {
	TenantID string `json:"tenant_id"`
	AgentID  string `json:"agent_id"`
}

// KIFF is the part of KIFF a draft needs.
type KIFF interface {
	// Submit creates a draft with the submitter's key, and returns it with
	// its claim secret.
	Submit(ctx context.Context, key string, body json.RawMessage) (Draft, string, error)
	// Claim takes the key of a confirmed draft with its claim secret, once.
	// ErrNotClaimable while the owner hasn't confirmed it; ErrExpired once
	// it expired.
	Claim(ctx context.Context, draft, secret string) (Claimed, error)
	// Me says who key speaks for.
	Me(ctx context.Context, key string) (Me, error)
}

var (
	// ErrNotFound: no draft submitted here by that id, for this tenant.
	ErrNotFound = errors.New("drafts: no such draft")
	// ErrNotClaimable: KIFF won't give the draft's key now: the owner hasn't
	// confirmed it yet, or it was already claimed or discarded.
	ErrNotClaimable = errors.New("drafts: KIFF can't give the draft's key now: not confirmed yet, or already claimed or discarded")
	// ErrExpired: the draft expired in KIFF before it was claimed.
	ErrExpired = errors.New("drafts: the draft expired; it must be submitted again")
	// ErrTaken: a key is already stored under that reference.
	ErrTaken = errors.New("drafts: a key is already stored under that reference")
	// ErrOtherRef: the draft was claimed into another reference.
	ErrOtherRef = errors.New("drafts: the draft's key was claimed into another reference")
	// ErrMismatch: KIFF's key is not bound to the draft's agent on this
	// tenant. It is not stored.
	ErrMismatch = errors.New("drafts: the key KIFF minted is not the draft's agent's on this tenant")
)

// Service submits drafts and claims their keys.
type Service struct {
	Store Store
	Keys  Keys
	KIFF  KIFF
}

// Submit creates the draft with the submitting agent's key and keeps its
// claim secret. The draft it returns carries no secret.
func (s *Service) Submit(ctx context.Context, tenant, key string, body json.RawMessage) (Draft, error) {
	d, secret, err := s.KIFF.Submit(ctx, key, body)
	if err != nil {
		return Draft{}, err
	}
	if d.ID == "" || !strings.HasPrefix(secret, "kiff_claim_") {
		return Draft{}, errors.New("drafts: KIFF's answer has no draft id or claim secret")
	}
	if err := s.Store.Put(ctx, tenant, d.ID, Record{AgentID: d.AgentID, Secret: secret}); err != nil {
		return Draft{}, fmt.Errorf("drafts: keeping the claim secret: %w", err)
	}
	return d, nil
}

// Claim claims the key of the tenant's draft into keyRef and returns the
// draft's agent. Asking again with the same keyRef answers the same; with
// another, ErrOtherRef.
//
// KIFF mints the key once, so everything that can refuse is checked first:
// the draft is this tenant's and unclaimed, and keyRef is free. Then the key
// is claimed, checked to be the draft's agent's on this tenant, stored, and
// the secret dropped.
func (s *Service) Claim(ctx context.Context, tenant, draft, keyRef string) (string, error) {
	rec, err := s.Store.Get(ctx, tenant, draft)
	if err != nil {
		return "", err
	}
	if rec.Claimed {
		if rec.KeyRef != keyRef {
			return "", ErrOtherRef
		}
		return rec.AgentID, nil
	}
	if held, found, err := s.Keys.Key(ctx, tenant, keyRef); err != nil {
		return "", err
	} else if found {
		// A claim that stored the key and stopped before recording itself
		// left this draft's own key here: finish it. Anything else is
		// another agent's key, and stays.
		if me, err := s.KIFF.Me(ctx, held); err == nil && me.TenantID == tenant && me.AgentID != "" && me.AgentID == rec.AgentID {
			return rec.AgentID, s.Store.Put(ctx, tenant, draft, Record{AgentID: rec.AgentID, KeyRef: keyRef, Claimed: true})
		}
		return "", ErrTaken
	}
	c, err := s.KIFF.Claim(ctx, draft, rec.Secret)
	if err != nil {
		return "", err
	}
	me, err := s.KIFF.Me(ctx, c.Key)
	if err != nil {
		return "", fmt.Errorf("drafts: asking KIFF who the new key is: %w", err)
	}
	if me.TenantID != tenant || me.AgentID == "" || me.AgentID != rec.AgentID || c.AgentID != rec.AgentID {
		_ = s.Store.Put(ctx, tenant, draft, Record{AgentID: rec.AgentID})
		return "", ErrMismatch
	}
	if err := s.Keys.Create(ctx, tenant, keyRef, c.Key); err != nil {
		return "", fmt.Errorf("drafts: storing the key: %w", err)
	}
	if err := s.Store.Put(ctx, tenant, draft, Record{AgentID: rec.AgentID, KeyRef: keyRef, Claimed: true}); err != nil {
		return "", fmt.Errorf("drafts: recording the claim: %w", err)
	}
	return rec.AgentID, nil
}
