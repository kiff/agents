package dynamodb

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"

	"github.com/kiff/agents/keys"
)

// AlertSecrets keeps each tenant's KIFF alert secret (what kiffalerts checks
// an alert's signature with) on the store's table, one item each, sealed by
// KMS, so it costs nothing of its own while it's kept:
//
//	secret   pk=A2|tenant  sk=alert  the secret, sealed in "sealed"
//	removed  pk=A2|tenant  sk=alert  "removed": the tenant's secret was removed
//
// The KIFF account that sends a tenant's alerts is the tenant itself (tafs
// cloud registers each account's alert target under its own tenant). tafs
// cloud writes it through the API (PUT and DELETE /tenant/alert-secret);
// kiffalerts only reads it.
//
// A removed secret leaves a mark, so it never comes back from where it was
// kept before (kiffalerts reads its old secret only while the table has
// nothing for the tenant), nor from a move (Create never writes over it).
// The mark holds the tenant alone. Put replaces it.
//
// It's sealed under the encryption context {purpose, tenant}, taken from the
// call, never from the item, so a secret copied to another tenant doesn't
// open. Without KMS nothing is written or read, and no error carries a
// secret.
type AlertSecrets struct {
	db    API
	table string
	kms   KMS
	keyID string
}

// ErrNoAlertSecret is a tenant with no alert secret kept here.
var ErrNoAlertSecret = errors.New("dynamodb: no alert secret")

// ErrAlertSecretRemoved is a tenant whose alert secret was removed: no
// alert of theirs is checked, by this secret or an older one.
var ErrAlertSecretRemoved = errors.New("dynamodb: the alert secret was removed")

// NewAlertSecrets returns the alert secrets on table, sealed with the KMS
// key keyID.
func NewAlertSecrets(db API, table string, k KMS, keyID string) *AlertSecrets {
	return &AlertSecrets{db: db, table: table, kms: k, keyID: keyID}
}

func alertPK(tenant string) string { return key("A2", tenant) }

const alertSK = "alert"

func alertContext(tenant string) map[string]string {
	return map[string]string{"purpose": "kiff/agents alert secret v1", "tenant": tenant}
}

func (a *AlertSecrets) sealed(ctx context.Context, tenant, secret string) (map[string]types.AttributeValue, error) {
	if err := keys.Valid(tenant, alertSK); err != nil {
		return nil, err
	}
	if secret == "" {
		return nil, errors.New("dynamodb: no alert secret given")
	}
	out, err := a.kms.Encrypt(ctx, &kms.EncryptInput{KeyId: &a.keyID, Plaintext: []byte(secret), EncryptionContext: alertContext(tenant)})
	if err != nil {
		return nil, fmt.Errorf("dynamodb: sealing the alert secret: %w", kmsErr(err))
	}
	item := pkey(alertPK(tenant), alertSK)
	item["sealed"] = &types.AttributeValueMemberB{Value: out.CiphertextBlob}
	return item, nil
}

// Get opens the tenant's alert secret; ErrNoAlertSecret if none is kept,
// ErrAlertSecretRemoved if it was removed.
func (a *AlertSecrets) Get(ctx context.Context, tenant string) (string, error) {
	if err := keys.Valid(tenant, alertSK); err != nil {
		return "", err
	}
	out, err := a.db.GetItem(ctx, &ddb.GetItemInput{TableName: &a.table, Key: pkey(alertPK(tenant), alertSK), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return "", fmt.Errorf("dynamodb: reading the alert secret: %w", err)
	}
	if out.Item == nil {
		return "", ErrNoAlertSecret
	}
	if _, removed := out.Item["removed"]; removed {
		return "", ErrAlertSecretRemoved
	}
	sealed, _ := out.Item["sealed"].(*types.AttributeValueMemberB)
	if sealed == nil || len(sealed.Value) == 0 {
		return "", errors.New("dynamodb: the alert secret is not sealed")
	}
	opened, err := a.kms.Decrypt(ctx, &kms.DecryptInput{KeyId: &a.keyID, CiphertextBlob: sealed.Value, EncryptionContext: alertContext(tenant)})
	if err != nil {
		return "", fmt.Errorf("dynamodb: opening the alert secret: %w", kmsErr(err))
	}
	if len(opened.Plaintext) == 0 {
		return "", errors.New("dynamodb: the item holds no alert secret")
	}
	return string(opened.Plaintext), nil
}

// Put stores the tenant's alert secret, replacing any.
func (a *AlertSecrets) Put(ctx context.Context, tenant, secret string) error {
	item, err := a.sealed(ctx, tenant, secret)
	if err != nil {
		return err
	}
	if _, err := a.db.PutItem(ctx, &ddb.PutItemInput{TableName: &a.table, Item: item}); err != nil {
		return fmt.Errorf("dynamodb: writing the alert secret: %w", err)
	}
	return nil
}

// Create stores the tenant's alert secret where nothing is kept, a removed
// mark included: a migration's step, so a secret written through the API
// meanwhile is never written over, and a removed one never comes back.
// copied is false when something was there.
func (a *AlertSecrets) Create(ctx context.Context, tenant, secret string) (copied bool, err error) {
	item, err := a.sealed(ctx, tenant, secret)
	if err != nil {
		return false, err
	}
	_, err = a.db.PutItem(ctx, &ddb.PutItemInput{TableName: &a.table, Item: item, ConditionExpression: aws.String("attribute_not_exists(pk)")})
	var taken *types.ConditionalCheckFailedException
	if errors.As(err, &taken) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("dynamodb: writing the alert secret: %w", err)
	}
	return true, nil
}

// Remove replaces the tenant's alert secret with a removed mark; none there
// is not an error.
func (a *AlertSecrets) Remove(ctx context.Context, tenant string) error {
	if err := keys.Valid(tenant, alertSK); err != nil {
		return err
	}
	item := pkey(alertPK(tenant), alertSK)
	item["removed"] = &types.AttributeValueMemberBOOL{Value: true}
	if _, err := a.db.PutItem(ctx, &ddb.PutItemInput{TableName: &a.table, Item: item}); err != nil {
		return fmt.Errorf("dynamodb: removing the alert secret: %w", err)
	}
	return nil
}

// Erase is Remove, when the tenant's account is deleted: the mark stays,
// so the account's old secret, if it's still kept, never checks an alert.
func (a *AlertSecrets) Erase(ctx context.Context, tenant string) error { return a.Remove(ctx, tenant) }
