package secretsmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/kiff/agents/server/kiffalert"
)

// Accounts is kiffalert.Accounts on Secrets Manager: each KIFF account that
// sends alerts here has a secret named prefix + its account, which holds
//
//	{"tenant": "t1", "secret": "<the alert target's signing secret>"}
//
// The prefix must be outside the agents' key prefix (Secrets.Prefix): an
// agent's key reference must never name a signing secret.
type Accounts struct {
	api API
	// Prefix starts every account's secret's name, such as "kiff-accounts/".
	Prefix string
}

var _ kiffalert.Accounts = (*Accounts)(nil)

// NewAccounts returns Accounts that read the secrets named under prefix.
func NewAccounts(api API, prefix string) *Accounts {
	return &Accounts{api: api, Prefix: prefix}
}

// Account returns the tenant that takes alerts from kiffAccount, and the
// signing secret they are checked with. An account that is no single name
// segment, or has no secret, is kiffalert.ErrUnknownAccount. No error carries
// the signing secret.
func (a *Accounts) Account(ctx context.Context, kiffAccount string) (tenant, secret string, err error) {
	if !segment(kiffAccount) || len(a.Prefix)+len(kiffAccount) > maxName {
		return "", "", kiffalert.ErrUnknownAccount
	}
	name := a.Prefix + kiffAccount
	out, err := a.api.GetSecretValue(ctx, &sm.GetSecretValueInput{SecretId: aws.String(name)})
	var missing *types.ResourceNotFoundException
	if errors.As(err, &missing) {
		return "", "", kiffalert.ErrUnknownAccount
	}
	if err != nil {
		return "", "", fmt.Errorf("secretsmanager: reading the KIFF account: %w", err)
	}
	// As for keys: only the secret by this very name will do.
	if aws.ToString(out.Name) != name {
		return "", "", kiffalert.ErrUnknownAccount
	}
	var v struct {
		Tenant string `json:"tenant"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal([]byte(aws.ToString(out.SecretString)), &v); err != nil {
		return "", "", errors.New("secretsmanager: the KIFF account's secret is not {\"tenant\", \"secret\"}")
	}
	if !segment(v.Tenant) || v.Secret == "" {
		return "", "", errors.New("secretsmanager: the KIFF account's secret names no tenant or no signing secret")
	}
	return v.Tenant, v.Secret, nil
}

// List is every account with a secret under a.Prefix that isn't being
// deleted: what a migration copies to the table. A name past the prefix
// that isn't one segment is left out.
func (a *Accounts) List(ctx context.Context, api ListAPI) ([]string, error) {
	in := &sm.ListSecretsInput{Filters: []types.Filter{{Key: types.FilterNameStringTypeName, Values: []string{a.Prefix}}}}
	var out []string
	for {
		page, err := api.ListSecrets(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("secretsmanager: listing the accounts' secrets: %w", err)
		}
		for _, e := range page.SecretList {
			name := aws.ToString(e.Name)
			if e.DeletedDate != nil || !strings.HasPrefix(name, a.Prefix) || !segment(strings.TrimPrefix(name, a.Prefix)) {
				continue
			}
			out = append(out, strings.TrimPrefix(name, a.Prefix))
		}
		if page.NextToken == nil {
			return out, nil
		}
		in.NextToken = page.NextToken
	}
}
