package main

import (
	"context"
	"errors"

	"github.com/kiff/agents/keys"
	"github.com/kiff/agents/server/kiffalert"
	"github.com/kiff/agents/store/dynamodb"
)

// alertSecret is a tenant's alert secret as the table keeps it
// (dynamodb.AlertSecrets).
type alertSecret interface {
	Get(ctx context.Context, tenant string) (string, error)
}

// tableFirst reads an account's alert secret from the table, where tafs
// cloud writes it through the API, and from its old secret only while the
// table has nothing for it: a removed secret's mark answers unknown. The KIFF account that sends a tenant's alerts is the
// tenant itself.
type tableFirst struct {
	table alertSecret
	old   kiffalert.Accounts
}

func (t tableFirst) Account(ctx context.Context, kiffAccount string) (tenant, secret string, err error) {
	if keys.Valid(kiffAccount, "alert") != nil {
		return "", "", kiffalert.ErrUnknownAccount
	}
	secret, err = t.table.Get(ctx, kiffAccount)
	switch {
	case err == nil:
		return kiffAccount, secret, nil
	case errors.Is(err, dynamodb.ErrNoAlertSecret):
		return t.old.Account(ctx, kiffAccount)
	case errors.Is(err, dynamodb.ErrAlertSecretRemoved):
		// Removed: never the old secret, which may still be kept.
		return "", "", kiffalert.ErrUnknownAccount
	default:
		return "", "", err
	}
}
