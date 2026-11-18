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
	RestoreSecret(context.Context, *sm.RestoreSecretInput, ...func(*sm.Options)) (*sm.RestoreSecretOutput, error)
}

// Eraser deletes what a tenant keeps in Secrets Manager under kiff/agents'
// own prefixes, when its account is deleted (kiff/tafs#185): its sealing
// key, which leaves every read sealed under it unreadable, and its drafts'
// claim records, and the tafs' KIFF keys' old secrets while keys move to
// the table. Each is deleted at once, with no recovery window, a key's old
// secret retired after its move too (package keys). The
// account's alert secret is tafs cloud's, which writes it and deletes it.
type Eraser struct {
	api           EraseAPI
	SealingPrefix string // such as "agents-sealing/"; empty: none kept
	DraftsPrefix  string // such as "agents-drafts/"; empty: none kept
	// KeysPrefix is the tafs' KIFF keys' old secrets, such as "agents/",
	// while keys move to the table (package keys); empty: none erased here.
	KeysPrefix string
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
		// A tenant is one segment, so this prefix names its own only.
		found, _, err := e.under(ctx, e.DraftsPrefix+tenant+"/")
		if err != nil {
			return err
		}
		names = append(names, found...)
	}
	if e.KeysPrefix != "" {
		found, retired, err := e.under(ctx, e.KeysPrefix+tenant+"/")
		if err != nil {
			return err
		}
		for i, n := range found {
			if !retired[i] {
				names = append(names, n)
			} else if err := purge(ctx, e.api, n); err != nil {
				return fmt.Errorf("secretsmanager: %w", err)
			}
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

// under lists the secrets whose names start with prefix, those scheduled
// for deletion too, and which of them are.
func (e *Eraser) under(ctx context.Context, prefix string) (names []string, retired []bool, err error) {
	in := &sm.ListSecretsInput{
		Filters:                []types.Filter{{Key: types.FilterNameStringTypeName, Values: []string{prefix}}},
		IncludePlannedDeletion: aws.Bool(true),
	}
	for {
		out, err := e.api.ListSecrets(ctx, in)
		if err != nil {
			return nil, nil, fmt.Errorf("secretsmanager: listing the tenant's secrets: %w", err)
		}
		for _, s := range out.SecretList {
			// The name filter matches a prefix; check it, so nothing
			// outside the tenant's own is ever deleted.
			if n := aws.ToString(s.Name); len(n) > len(prefix) && n[:len(prefix)] == prefix {
				names = append(names, n)
				retired = append(retired, s.DeletedDate != nil)
			}
		}
		if out.NextToken == nil {
			return names, retired, nil
		}
		in.NextToken = out.NextToken
	}
}
