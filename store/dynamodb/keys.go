package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"

	"github.com/kiff/agents/keys"
)

// Keys is a keys.Store on the store's table: the tafs' KIFF keys, one item
// each, sealed by their own KMS key (not the drafts' one), so a key costs
// nothing of its own while it's kept.
//
//	key      pk=K2|tenant  sk=ref  the key, sealed in "sealed"
//	removed  pk=K2|tenant  sk=ref  "removed"; "until" and "ttl" once
//	                               nothing of the key is left elsewhere
//
// A key is sealed with kms:Encrypt under the encryption context {purpose,
// tenant, ref}, taken from the call, never from the item, so a key moved to
// another tenant or reference doesn't open. Without KMS nothing is written
// or read: a failure is an error, never a key in clear, and no error carries
// one.
//
// A removed key's mark stops a key coming back: from its old secret if that
// wasn't deleted (the mark is then kept), and from a move that read the old
// secret just before the removal (the mark outlives it by RemovedFor).
type Keys struct {
	db    API
	table string
	kms   KMS
	keyID string
	now   func() time.Time
	// RemovedFor is how long a settled removal's mark is kept.
	RemovedFor time.Duration
}

var _ keys.Store = (*Keys)(nil)

// KeyRemovedFor is how long a settled removal's mark is kept, by default.
const KeyRemovedFor = 24 * time.Hour

// NewKeys returns the keys store on table, sealing with the KMS key keyID.
func NewKeys(db API, table string, k KMS, keyID string) *Keys {
	return &Keys{db: db, table: table, kms: k, keyID: keyID, now: time.Now, RemovedFor: KeyRemovedFor}
}

// WithClock is k with the clock now, for tests.
func (k *Keys) WithClock(now func() time.Time) *Keys { c := *k; c.now = now; return &c }

func keysPK(tenant string) string { return key("K2", tenant) }

// keyContext is what a key is sealed to.
func keyContext(tenant, ref string) map[string]string {
	return map[string]string{"purpose": "kiff/agents taf key v1", "tenant": tenant, "ref": ref}
}

// sealed is the item holding key, sealed.
func (k *Keys) sealed(ctx context.Context, tenant, ref, key string) (map[string]types.AttributeValue, error) {
	if err := keys.Valid(tenant, ref); err != nil {
		return nil, err
	}
	if key == "" {
		return nil, errors.New("dynamodb: no key")
	}
	out, err := k.kms.Encrypt(ctx, &kms.EncryptInput{KeyId: &k.keyID, Plaintext: []byte(key), EncryptionContext: keyContext(tenant, ref)})
	if err != nil {
		return nil, fmt.Errorf("dynamodb: sealing the key: %w", kmsErr(err))
	}
	item := pkey(keysPK(tenant), ref)
	item["sealed"] = &types.AttributeValueMemberB{Value: out.CiphertextBlob}
	return item, nil
}

// Get opens the tenant's key by ref; keys.ErrNotFound if none, and
// keys.ErrRemoved if it was removed.
func (k *Keys) Get(ctx context.Context, tenant, ref string) (string, error) {
	if err := keys.Valid(tenant, ref); err != nil {
		return "", err
	}
	out, err := k.db.GetItem(ctx, &ddb.GetItemInput{TableName: &k.table, Key: pkey(keysPK(tenant), ref), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return "", fmt.Errorf("dynamodb: reading the key: %w", err)
	}
	if out.Item == nil {
		return "", keys.ErrNotFound
	}
	if _, removed := out.Item["removed"]; removed {
		if until := getN(out.Item, "until"); until != 0 && !k.now().Before(time.Unix(until, 0)) {
			// A settled removal's mark past its time: nothing is left anywhere.
			return "", keys.ErrNotFound
		}
		return "", keys.ErrRemoved
	}
	sealed, _ := out.Item["sealed"].(*types.AttributeValueMemberB)
	if sealed == nil || len(sealed.Value) == 0 {
		return "", errors.New("dynamodb: the key is not sealed")
	}
	opened, err := k.kms.Decrypt(ctx, &kms.DecryptInput{KeyId: &k.keyID, CiphertextBlob: sealed.Value, EncryptionContext: keyContext(tenant, ref)})
	if err != nil {
		return "", fmt.Errorf("dynamodb: opening the key: %w", kmsErr(err))
	}
	if len(opened.Plaintext) == 0 {
		return "", errors.New("dynamodb: the item holds no key")
	}
	return string(opened.Plaintext), nil
}

// Create stores key where nothing is, a removed key's mark included;
// keys.ErrTaken otherwise.
func (k *Keys) Create(ctx context.Context, tenant, ref, key string) error {
	item, err := k.sealed(ctx, tenant, ref, key)
	if err != nil {
		return err
	}
	_, err = k.db.PutItem(ctx, &ddb.PutItemInput{TableName: &k.table, Item: item, ConditionExpression: aws.String("attribute_not_exists(pk)")})
	var taken *types.ConditionalCheckFailedException
	if errors.As(err, &taken) {
		return keys.ErrTaken
	}
	if err != nil {
		return fmt.Errorf("dynamodb: writing the key: %w", err)
	}
	return nil
}

// Put stores key, replacing a key or a removed key's mark.
func (k *Keys) Put(ctx context.Context, tenant, ref, key string) error {
	item, err := k.sealed(ctx, tenant, ref, key)
	if err != nil {
		return err
	}
	if _, err := k.db.PutItem(ctx, &ddb.PutItemInput{TableName: &k.table, Item: item}); err != nil {
		return fmt.Errorf("dynamodb: writing the key: %w", err)
	}
	return nil
}

// Remove replaces the key with a removed key's mark: kept while settled is
// false, and for RemovedFor once it is.
func (k *Keys) Remove(ctx context.Context, tenant, ref string, settled bool) error {
	if err := keys.Valid(tenant, ref); err != nil {
		return err
	}
	item := pkey(keysPK(tenant), ref)
	item["removed"] = &types.AttributeValueMemberBOOL{Value: true}
	if settled {
		until := k.now().Add(k.RemovedFor)
		item["until"] = n(until.Unix())
		item["ttl"] = ttl(until)
	}
	if _, err := k.db.PutItem(ctx, &ddb.PutItemInput{TableName: &k.table, Item: item}); err != nil {
		return fmt.Errorf("dynamodb: removing the key: %w", err)
	}
	return nil
}

// Erase deletes every key of the tenant and their marks, when its account
// is deleted.
func (k *Keys) Erase(ctx context.Context, tenant string) error {
	if tenant == "" {
		return errors.New("dynamodb: erasing keys needs a tenant")
	}
	in := &ddb.QueryInput{
		TableName:                 &k.table,
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(keysPK(tenant))},
		ProjectionExpression:      aws.String("pk, sk"),
		ConsistentRead:            aws.Bool(true),
	}
	for {
		page, err := k.db.Query(ctx, in)
		if err != nil {
			return fmt.Errorf("dynamodb: listing the tenant's keys: %w", err)
		}
		for _, it := range page.Items {
			if _, err := k.db.DeleteItem(ctx, &ddb.DeleteItemInput{TableName: &k.table, Key: pkey(getS(it, "pk"), getS(it, "sk"))}); err != nil {
				return fmt.Errorf("dynamodb: erasing a key: %w", err)
			}
		}
		if len(page.LastEvaluatedKey) == 0 {
			return nil
		}
		in.ExclusiveStartKey = page.LastEvaluatedKey
	}
}
