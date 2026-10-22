package secretsmanager

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// EraseAPI is the part of the Secrets Manager client Eraser uses.
type EraseAPI interface {
	ListSecrets(context.Context, *sm.ListSecretsInput, ...func(*sm.Options)) (*sm.ListSecretsOutput, error)
	DeleteSecret(context.Context, *sm.DeleteSecretInput, ...func(*sm.Options)) (*sm.DeleteSecretOutput, error)
}

// Eraser deletes what a tenant keeps in Secrets Manager under kiff/agents'
// own prefixes, when its account is deleted (kiff/tafs#185): its sealing
// key, which leaves every read sealed under it unreadable, and its drafts'
// claim records. Each is deleted at once, with no recovery window. The
// tafs' KIFF keys and the account's alert secret are tafs cloud's, which
// writes them and deletes them.
type Eraser struct {
	api           EraseAPI
	SealingPrefix string // such as "agents-sealing/"; empty: none kept
	DraftsPrefix  string // such as "agent-drafts/"; empty: none kept
}

// NewEraser returns an Eraser for the secrets under those prefixes.
func NewEraser(api EraseAPI, sealingPrefix, draftsPrefix string) *Eraser {
	return &Eraser{api: api, SealingPrefix: sealingPrefix, DraftsPrefix: draftsPrefix}
}

// Erase deletes the tenant's sealing key and drafts' claim records. A
// secret already gone is not an error, so it is safe to repeat.
func (e *Eraser) Erase(ctx context.Context, tenant string) error {
	if !segment(tenant) {
		return errors.New("secretsmanager: a tenant is one name segment")
	}
	var names []string
	if e.SealingPrefix != "" {
		names = append(names, e.SealingPrefix+tenant)
	}
	if e.DraftsPrefix != "" {
		// A tenant is one segment, so this prefix names its drafts only.
		prefix := e.DraftsPrefix + tenant + "/"
		in := &sm.ListSecretsInput{Filters: []types.Filter{{Key: types.FilterNameStringTypeName, Values: []string{prefix}}}}
		for {
			out, err := e.api.ListSecrets(ctx, in)
			if err != nil {
				return fmt.Errorf("secretsmanager: listing the tenant's drafts: %w", err)
			}
			for _, s := range out.SecretList {
				// The name filter matches a prefix; check it, so nothing
				// outside the tenant's drafts is ever deleted.
				if n := aws.ToString(s.Name); len(n) > len(prefix) && n[:len(prefix)] == prefix {
					names = append(names, n)
				}
			}
			if out.NextToken == nil {
				break
			}
			in.NextToken = out.NextToken
		}
	}
	for _, n := range names {
		_, err := e.api.DeleteSecret(ctx, &sm.DeleteSecretInput{SecretId: aws.String(n), ForceDeleteWithoutRecovery: aws.Bool(true)})
		var missing *types.ResourceNotFoundException
		if err != nil && !errors.As(err, &missing) {
			return fmt.Errorf("secretsmanager: deleting %s: %w", n, err)
		}
	}
	return nil
}
