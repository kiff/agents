package main

import (
	"context"
	"errors"
	"testing"

	"github.com/kiff/agents/server/kiffalert"
	"github.com/kiff/agents/store/dynamodb"
)

type tableSecrets map[string]string

func (t tableSecrets) Get(_ context.Context, tenant string) (string, error) {
	if v, ok := t[tenant]; ok {
		if v == "fail" {
			return "", errors.New("kms: throttled")
		}
		return v, nil
	}
	return "", dynamodb.ErrNoAlertSecret
}

type oldAccounts map[string][2]string

func (o oldAccounts) Account(_ context.Context, account string) (string, string, error) {
	if v, ok := o[account]; ok {
		return v[0], v[1], nil
	}
	return "", "", kiffalert.ErrUnknownAccount
}

// The table's secret wins; with none there the old secret answers; a table
// that fails is an error, never the old (possibly stale) secret; an account
// that isn't one segment is unknown before anything is read.
func TestAnAlertSecretIsReadFromTheTableFirst(t *testing.T) {
	a := tableFirst{
		table: tableSecrets{"t1": "whsec_new", "t3": "fail"},
		old:   oldAccounts{"t1": {"t1", "whsec_old"}, "t2": {"t2", "whsec_two"}, "t3": {"t3", "whsec_three"}},
	}
	if tenant, secret, err := a.Account(ctx, "t1"); err != nil || tenant != "t1" || secret != "whsec_new" {
		t.Errorf("t1: %q %q %v", tenant, secret, err)
	}
	if tenant, secret, err := a.Account(ctx, "t2"); err != nil || tenant != "t2" || secret != "whsec_two" {
		t.Errorf("t2, only its old secret: %q %q %v", tenant, secret, err)
	}
	if _, secret, err := a.Account(ctx, "t3"); err == nil || secret != "" {
		t.Errorf("t3, the table failing: %q %v", secret, err)
	}
	if _, _, err := a.Account(ctx, "t4"); !errors.Is(err, kiffalert.ErrUnknownAccount) {
		t.Errorf("t4, nowhere: %v", err)
	}
	for _, bad := range []string{"", "t1/x", "..", "t1|x"} {
		if _, _, err := a.Account(ctx, bad); !errors.Is(err, kiffalert.ErrUnknownAccount) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}
