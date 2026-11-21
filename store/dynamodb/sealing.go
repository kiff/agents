package dynamodb

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"

	"github.com/kiff/agents/keys"
)

// SealingKeys keeps each tenant's data key for sealing what a chat keeps of
// its reads (session.KeySealer, kiff/agents#172) on the store's table, one
// item each, sealed by KMS, so a key costs nothing of its own while it's
// kept:
//
//	key  pk=S2|tenant  sk=sealing  the 32 bytes, sealed in "sealed"
//
// Its own partition, apart from the tafs' keys (K2), so no key reference can
// name it. It's sealed under the encryption context {purpose, tenant}, taken
// from the call, never from the item.
//
// A tenant whose key is still an old secret keeps those same bytes: Old reads
// it and the key is written here create-only, never made anew while an old
// one may exist, since every read sealed under the old key would be lost.
// The old secret is left as it is, deleted on its own once each key is here.
type SealingKeys struct {
	db    API
	table string
	kms   KMS
	keyID string
	// Old is the tenant's key from where it was kept before, or
	// ErrSealingKeyNotFound when it has none there; nil: nowhere.
	Old func(ctx context.Context, tenant string) ([]byte, error)
}

// ErrSealingKeyNotFound is a tenant with no sealing key kept.
var ErrSealingKeyNotFound = errors.New("dynamodb: no sealing key")

// NewSealingKeys returns the sealing keys on table, sealed with the KMS key
// keyID.
func NewSealingKeys(db API, table string, k KMS, keyID string) *SealingKeys {
	return &SealingKeys{db: db, table: table, kms: k, keyID: keyID}
}

func sealingPK(tenant string) string { return key("S2", tenant) }

const sealingSK = "sealing"

func sealingContext(tenant string) map[string]string {
	return map[string]string{"purpose": "kiff/agents sealing key v1", "tenant": tenant}
}

// Key is the tenant's data key: the one kept here, else its old one moved
// here, else one made for it. Two invocations making or moving it at once
// both end up with the one written first.
func (k *SealingKeys) Key(ctx context.Context, tenant string) ([]byte, error) {
	if err := keys.Valid(tenant, sealingSK); err != nil {
		return nil, err
	}
	if key, err := k.get(ctx, tenant); !errors.Is(err, ErrSealingKeyNotFound) {
		return key, err
	}
	var key []byte
	if k.Old != nil {
		old, err := k.Old(ctx, tenant)
		switch {
		case err == nil:
			key = old
		case !errors.Is(err, ErrSealingKeyNotFound):
			// Unread, it may still exist: a key made now would lose
			// everything sealed under it.
			return nil, fmt.Errorf("dynamodb: reading the tenant's old sealing key: %w", err)
		}
	}
	if key == nil {
		key = make([]byte, 32)
		_, _ = rand.Read(key) // crypto/rand.Read never fails on supported platforms
	}
	if len(key) != 32 {
		return nil, errors.New("dynamodb: a sealing key is 32 bytes")
	}
	switch err := k.create(ctx, tenant, key); {
	case err == nil:
		return key, nil
	case errors.Is(err, errSealingTaken):
		return k.get(ctx, tenant) // written meanwhile
	default:
		return nil, err
	}
}

var errSealingTaken = errors.New("dynamodb: the sealing key was written meanwhile")

func (k *SealingKeys) get(ctx context.Context, tenant string) ([]byte, error) {
	out, err := k.db.GetItem(ctx, &ddb.GetItemInput{TableName: &k.table, Key: pkey(sealingPK(tenant), sealingSK), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return nil, fmt.Errorf("dynamodb: reading the sealing key: %w", err)
	}
	if out.Item == nil {
		return nil, ErrSealingKeyNotFound
	}
	sealed, _ := out.Item["sealed"].(*types.AttributeValueMemberB)
	if sealed == nil || len(sealed.Value) == 0 {
		return nil, errors.New("dynamodb: the sealing key is not sealed")
	}
	opened, err := k.kms.Decrypt(ctx, &kms.DecryptInput{KeyId: &k.keyID, CiphertextBlob: sealed.Value, EncryptionContext: sealingContext(tenant)})
	if err != nil {
		return nil, fmt.Errorf("dynamodb: opening the sealing key: %w", kmsErr(err))
	}
	if len(opened.Plaintext) != 32 {
		return nil, errors.New("dynamodb: the sealing key is not 32 bytes")
	}
	return opened.Plaintext, nil
}

func (k *SealingKeys) create(ctx context.Context, tenant string, key []byte) error {
	out, err := k.kms.Encrypt(ctx, &kms.EncryptInput{KeyId: &k.keyID, Plaintext: key, EncryptionContext: sealingContext(tenant)})
	if err != nil {
		return fmt.Errorf("dynamodb: sealing the sealing key: %w", kmsErr(err))
	}
	item := pkey(sealingPK(tenant), sealingSK)
	item["sealed"] = &types.AttributeValueMemberB{Value: out.CiphertextBlob}
	_, err = k.db.PutItem(ctx, &ddb.PutItemInput{TableName: &k.table, Item: item, ConditionExpression: aws.String("attribute_not_exists(pk)")})
	var taken *types.ConditionalCheckFailedException
	if errors.As(err, &taken) {
		return errSealingTaken
	}
	if err != nil {
		return fmt.Errorf("dynamodb: writing the sealing key: %w", err)
	}
	return nil
}

// Erase deletes the tenant's sealing key, when its account is deleted:
// every read sealed under it is then unreadable.
func (k *SealingKeys) Erase(ctx context.Context, tenant string) error {
	if err := keys.Valid(tenant, sealingSK); err != nil {
		return err
	}
	if _, err := k.db.DeleteItem(ctx, &ddb.DeleteItemInput{TableName: &k.table, Key: pkey(sealingPK(tenant), sealingSK)}); err != nil {
		return fmt.Errorf("dynamodb: erasing the sealing key: %w", err)
	}
	return nil
}
