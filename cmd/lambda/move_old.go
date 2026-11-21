package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/kiff/agents/registry/secretsmanager"
	"github.com/kiff/agents/server/kiffalert"
)

// moveCounts is what a move_old_keys run did, by key: never a key itself.
type moveCounts struct {
	// Listed is how many old secrets there were (being deleted left out).
	Listed int `json:"listed"`
	// Copied is how many were written to the table.
	Copied int `json:"copied"`
	// Already is how many the table had (or had removed): left as they are.
	Already int `json:"already"`
	// Failed is how many couldn't be read or written; each is logged.
	Failed int `json:"failed"`
}

// keyCopier copies one key's old secret into the table (tafkeys.Moving).
type keyCopier interface {
	Copy(ctx context.Context, tenant, ref string) (copied bool, err error)
}

// moveOldKeys is the move_old_keys event: every tafs' key still kept as its
// own secret is copied to the sealed table, and no old secret is touched, so
// the old ones can be deleted on their own once each key is read from the
// table. A key the table already has is left as it is. Run again, it copies
// only what's missing.
func moveOldKeys(list func(context.Context) ([]secretsmanager.OldKey, error), keys keyCopier, log *slog.Logger) func(context.Context) (moveCounts, error) {
	return func(ctx context.Context) (moveCounts, error) {
		old, err := list(ctx)
		if err != nil {
			return moveCounts{}, err
		}
		c := moveCounts{Listed: len(old)}
		var failed error
		for _, k := range old {
			copied, err := keys.Copy(ctx, k.Tenant, k.Ref)
			switch {
			case err != nil:
				c.Failed++
				failed = errors.Join(failed, err)
				log.WarnContext(ctx, "old key not moved", "tenant", k.Tenant, "ref", k.Ref, "err", err)
			case copied:
				c.Copied++
				log.InfoContext(ctx, "old key moved", "tenant", k.Tenant, "ref", k.Ref)
			default:
				c.Already++
			}
		}
		return c, failed
	}
}

// alertCopier writes a tenant's alert secret where none is kept
// (dynamodb.AlertSecrets).
type alertCopier interface {
	Create(ctx context.Context, tenant, secret string) (copied bool, err error)
}

// moveOldAccounts is the move_old_accounts event: each KIFF account's alert
// secret still kept as its own secret is copied to the sealed table,
// create-only, and no old secret is touched. Run it only once tafs cloud
// writes them through the API: a secret written to the old store after it
// would be hidden by the table's copy. An account whose secret names
// another tenant isn't copied: the table keeps a tenant's own.
func moveOldAccounts(list func(context.Context) ([]string, error), old kiffalert.Accounts, table alertCopier, log *slog.Logger) func(context.Context) (moveCounts, error) {
	return func(ctx context.Context) (moveCounts, error) {
		accounts, err := list(ctx)
		if err != nil {
			return moveCounts{}, err
		}
		c := moveCounts{Listed: len(accounts)}
		var failed error
		for _, account := range accounts {
			tenant, secret, err := old.Account(ctx, account)
			if err == nil && tenant != account {
				err = errors.New("the secret names another tenant than its account")
			}
			copied := false
			if err == nil {
				copied, err = table.Create(ctx, tenant, secret)
			}
			switch {
			case err != nil:
				c.Failed++
				failed = errors.Join(failed, fmt.Errorf("account %s: %w", account, err))
				log.WarnContext(ctx, "old alert secret not moved", "account", account, "err", err)
			case copied:
				c.Copied++
				log.InfoContext(ctx, "old alert secret moved", "account", account)
			default:
				c.Already++
			}
		}
		return c, failed
	}
}

// thenErase is erase, then next; with no erase, next alone.
func thenErase(erase, next func(context.Context, string) error) func(context.Context, string) error {
	if erase == nil {
		return next
	}
	return func(ctx context.Context, tenant string) error {
		if err := erase(ctx, tenant); err != nil {
			return err
		}
		return next(ctx, tenant)
	}
}
