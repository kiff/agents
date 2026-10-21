package secretsmanager

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// SealingKeys keeps each tenant's data key for sealing what a chat keeps of
// its reads (session.KeySealer, kiff/agents#172): a secret named prefix +
// tenant, holding 32 random bytes, made the first time the tenant needs one.
// Secrets Manager keeps it encrypted under KMS. Deleting it makes every read
// sealed under it unreadable.
//
// The prefix must be outside the agents' key prefix (Secrets.Prefix), so no
// agent's key reference can name a sealing key.
type SealingKeys struct {
	api    WriteAPI
	Prefix string // such as "agents-sealing/"
}

// NewSealingKeys returns sealing keys under prefix.
func NewSealingKeys(api WriteAPI, prefix string) *SealingKeys {
	return &SealingKeys{api: api, Prefix: prefix}
}

// Key is the tenant's data key, made if it has none. Two invocations making
// it at once both end up with the one Secrets Manager kept.
func (k *SealingKeys) Key(ctx context.Context, tenant string) ([]byte, error) {
	if !segment(tenant) {
		return nil, errors.New("secretsmanager: a tenant is one name segment")
	}
	name := k.Prefix + tenant
	if key, err := k.read(ctx, name); !errors.Is(err, ErrNotFound) {
		return key, err
	}
	fresh := make([]byte, 32)
	_, _ = rand.Read(fresh) // crypto/rand.Read never fails on supported platforms
	_, err := k.api.CreateSecret(ctx, &sm.CreateSecretInput{Name: aws.String(name),
		SecretString: aws.String(base64.StdEncoding.EncodeToString(fresh)),
		Description:  aws.String("the tenant's key for what its chats keep of their reads, sealed (kiff/agents#172)")})
	var exists *types.ResourceExistsException
	if errors.As(err, &exists) {
		return k.read(ctx, name) // made meanwhile
	}
	if err != nil {
		return nil, fmt.Errorf("secretsmanager: making the tenant's sealing key: %w", err)
	}
	return fresh, nil
}

func (k *SealingKeys) read(ctx context.Context, name string) ([]byte, error) {
	out, err := k.api.GetSecretValue(ctx, &sm.GetSecretValueInput{SecretId: aws.String(name)})
	var missing *types.ResourceNotFoundException
	if errors.As(err, &missing) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("secretsmanager: reading the tenant's sealing key: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(aws.ToString(out.SecretString))
	if err != nil || len(key) != 32 {
		return nil, errors.New("secretsmanager: the tenant's sealing key is not 32 bytes")
	}
	return key, nil
}
