// Package keys keeps the tafs' KIFF keys: a store behind kiff/agents that
// tafs cloud writes through two signed routes, and that runs read by
// reference (registry.Secrets).
//
// Keys move from a secret each (Secrets Manager) to sealed items in the
// table. Moving reads the new store first and falls back to the old secret,
// moving it on that first read; a write goes to the new store and retires
// the old secret, which can be restored for a week, and a removal deletes
// it at once. A removed key leaves a mark in the new store, so it never
// comes back from an old secret whose delete failed, or from a move that
// read it just before.
package keys

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kiff/agents/drafts"
	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/registry/secretsmanager"
)

// ErrNotFound means the tenant has no key by that reference. It is
// secretsmanager.ErrNotFound, which callers already check.
var ErrNotFound = secretsmanager.ErrNotFound

// ErrRemoved is a key that was removed: not found, for good.
var ErrRemoved = fmt.Errorf("%w: it was removed", ErrNotFound)

// ErrTaken is a create-only write where a key, or a removed key's mark,
// already is.
var ErrTaken = errors.New("keys: a key is already stored there")

// Store is the new store: one sealed key per tenant and reference.
type Store interface {
	// Get is the key; ErrNotFound if none, ErrRemoved if it was removed.
	Get(ctx context.Context, tenant, ref string) (string, error)
	// Create stores key only where nothing is, a removed key's mark
	// included; ErrTaken otherwise.
	Create(ctx context.Context, tenant, ref, key string) error
	// Put stores key, replacing what is there.
	Put(ctx context.Context, tenant, ref, key string) error
	// Remove replaces the key with a removed key's mark. settled: nothing of
	// it is left anywhere else, so the mark need only outlive a move in
	// flight; otherwise it is kept.
	Remove(ctx context.Context, tenant, ref string, settled bool) error
}

// Old is where keys were: a secret each.
type Old interface {
	// Key is the key; ErrNotFound if there is no secret, or one being
	// deleted.
	Key(ctx context.Context, tenant, ref string) (string, error)
	// Retire deletes the secret with a recovery window, so a move or a
	// replace can be undone for a while; none there, or one already being
	// deleted, is not an error.
	Retire(ctx context.Context, tenant, ref string) error
	// Delete deletes the secret at once, a retired one too; none there is
	// not an error.
	Delete(ctx context.Context, tenant, ref string) error
}

// Moving is the keys while they move: New is read first and written, Old
// only read, retired from and deleted from.
type Moving struct {
	New Store
	Old Old
	// Log records an old secret that wasn't retired or deleted, by
	// reference, never by value. Nil logs nothing.
	Log func(msg, tenant, ref string, err error)
}

var _ registry.Secrets = Moving{}

func (m Moving) log(msg, tenant, ref string, err error) {
	if m.Log != nil {
		m.Log(msg, tenant, ref, err)
	}
}

// Key is the tenant's key by ref. One found only in Old is moved: created
// in New, create-only, then its secret retired. If the create loses (a Put
// or a Remove came first), it is never retried over what won, the secret is
// still retired, and what won is answered.
func (m Moving) Key(ctx context.Context, tenant, ref string) (string, error) {
	if err := Valid(tenant, ref); err != nil {
		return "", err
	}
	key, err := m.New.Get(ctx, tenant, ref)
	if err == nil || errors.Is(err, ErrRemoved) || !errors.Is(err, ErrNotFound) || m.Old == nil {
		return key, err
	}
	key, err = m.Old.Key(ctx, tenant, ref)
	if err != nil {
		return "", err
	}
	switch err := m.New.Create(ctx, tenant, ref, key); {
	case err == nil:
	case errors.Is(err, ErrTaken):
		key, err = m.New.Get(ctx, tenant, ref)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return "", err
		}
		if err != nil {
			key = ""
		}
	default:
		// Nothing was written: the key read stands, and its secret stays.
		m.log("keys: a key wasn't moved", tenant, ref, err)
		return key, nil
	}
	if err := m.Old.Retire(ctx, tenant, ref); err != nil {
		m.log("keys: a moved key's old secret wasn't retired", tenant, ref, err)
	}
	if key == "" {
		return "", ErrRemoved
	}
	return key, nil
}

// Put stores the tenant's key by ref, replacing any, and retires its old
// secret.
func (m Moving) Put(ctx context.Context, tenant, ref, key string) error {
	if err := Valid(tenant, ref); err != nil {
		return err
	}
	if strings.TrimSpace(key) == "" {
		return errors.New("keys: no key")
	}
	if err := m.New.Put(ctx, tenant, ref, key); err != nil {
		return err
	}
	if m.Old != nil {
		if err := m.Old.Retire(ctx, tenant, ref); err != nil {
			m.log("keys: a replaced key's old secret wasn't retired", tenant, ref, err)
		}
	}
	return nil
}

// Remove removes the tenant's key by ref: its old secret first, deleted at
// once even if retired, then the key in New, replaced by a removed key's
// mark either way. If the secret wasn't deleted, the mark is kept, so the key reads as removed, and the
// error is answered for the caller to try again. If it never does, the
// secret stays, read by nothing here: the one-off that ends the move
// deletes the old secret under any mark that isn't settled.
func (m Moving) Remove(ctx context.Context, tenant, ref string) error {
	if err := Valid(tenant, ref); err != nil {
		return err
	}
	var oldErr error
	if m.Old != nil {
		oldErr = m.Old.Delete(ctx, tenant, ref)
	}
	if err := m.New.Remove(ctx, tenant, ref, oldErr == nil); err != nil {
		return err
	}
	if oldErr != nil {
		return fmt.Errorf("keys: the key's old secret: %w", oldErr)
	}
	return nil
}

// Claims is the keys as a draft's claim stores them (drafts.Keys): never
// over a key that is there, in either store.
type Claims struct{ Moving }

var _ drafts.Keys = Claims{}

// Key is the key stored under ref, if any; drafts.ErrTaken if it was
// removed, so nothing is claimed there again.
func (c Claims) Key(ctx context.Context, tenant, ref string) (string, bool, error) {
	key, err := c.Moving.Key(ctx, tenant, ref)
	switch {
	case err == nil:
		return key, true, nil
	case errors.Is(err, ErrRemoved):
		return "", false, drafts.ErrTaken
	case errors.Is(err, ErrNotFound):
		return "", false, nil
	}
	return "", false, err
}

// Create stores key under ref; drafts.ErrTaken if a key, a removed key's
// mark or an old secret is there.
func (c Claims) Create(ctx context.Context, tenant, ref, key string) error {
	if err := Valid(tenant, ref); err != nil {
		return err
	}
	if c.Old != nil {
		if _, err := c.Old.Key(ctx, tenant, ref); err == nil {
			return drafts.ErrTaken
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	if err := c.New.Create(ctx, tenant, ref, key); errors.Is(err, ErrTaken) {
		return drafts.ErrTaken
	} else if err != nil {
		return err
	}
	return nil
}

// allowed is what a segment may hold besides letters and digits, as the
// old secrets' names did.
const allowed = "_+=.@-"

// Valid is nil when tenant is one segment and ref one or more, split by
// '/': no reference reaches another tenant's keys.
func Valid(tenant, ref string) error {
	if !segment(tenant) {
		return fmt.Errorf("keys: a tenant is one segment of letters, digits or %q", allowed)
	}
	if len(ref) > 256 {
		return errors.New("keys: a key reference is at most 256 bytes")
	}
	for _, p := range strings.Split(ref, "/") {
		if !segment(p) {
			return fmt.Errorf("keys: a key reference is segments of letters, digits or %q, split by '/'", allowed)
		}
	}
	return nil
}

func segment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, c := range []byte(s) {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte(allowed, c) >= 0) {
			return false
		}
	}
	return true
}
