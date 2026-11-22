package dynamodb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"

	"github.com/kiff/agents/drafts"
)

// KMS is the part of the KMS client the drafts store seals with.
type KMS interface {
	Encrypt(context.Context, *kms.EncryptInput, ...func(*kms.Options)) (*kms.EncryptOutput, error)
	Decrypt(context.Context, *kms.DecryptInput, ...func(*kms.Options)) (*kms.DecryptOutput, error)
}

// Drafts is a drafts.Store on the store's table: one item per submitted
// draft, its Record sealed by one KMS key, so a record costs nothing of its
// own while it's kept and goes by itself when its time is up.
//
//	draft  pk=D|tenant  sk=draft  the Record, sealed in "sealed"; "until"
//	                              and "ttl" when it expires
//
// A record holds credentials for a while: the claim secret until the claim,
// and the minted KIFF key while a claim is halfway. It is sealed with
// kms:Encrypt under the encryption context {purpose, tenant, draft}, taken
// from the call, never from the item, so a record moved to another tenant or
// draft doesn't open. Without KMS nothing is written or read: a failure is an
// error, never a record in clear.
//
// How long a record is kept follows what it holds:
//   - unclaimed: UnclaimedFor, past KIFF's claim window (a day), after which
//     its claim secret can't be used;
//   - claimed: ClaimedFor, for a retried claim and the "set up" read;
//   - a live key held (a mismatch, or a claim halfway): no expiry, until the
//     key is revoked or stored, since it must never be lost track of.
//
// DynamoDB removes expired items late, so Get treats one past its "until"
// as gone, whether or not it is still in the table.
type Drafts struct {
	db    API
	table string
	kms   KMS
	keyID string
	now   func() time.Time
	// UnclaimedFor and ClaimedFor are how long an unclaimed and a claimed
	// record are kept.
	UnclaimedFor, ClaimedFor time.Duration
}

var (
	_ drafts.Store  = (*Drafts)(nil)
	_ drafts.Lister = (*Drafts)(nil)
)

// Kept for, by default.
const (
	DraftUnclaimedFor = 48 * time.Hour
	DraftClaimedFor   = 14 * 24 * time.Hour
)

// NewDrafts returns the drafts store on table, sealing with the KMS key keyID.
func NewDrafts(db API, table string, k KMS, keyID string) *Drafts {
	return &Drafts{db: db, table: table, kms: k, keyID: keyID, now: time.Now, UnclaimedFor: DraftUnclaimedFor, ClaimedFor: DraftClaimedFor}
}

// WithClock is d with the clock now, for tests.
func (d *Drafts) WithClock(now func() time.Time) *Drafts { c := *d; c.now = now; return &c }

func draftsPK(tenant string) string { return key("D", tenant) }

// draftContext is what a draft's record is sealed to.
func draftContext(tenant, draft string) map[string]string {
	return map[string]string{"purpose": "kiff/agents draft record v1", "tenant": tenant, "draft": draft}
}

// keepFor is how long r is kept; zero: until it no longer holds a live key.
func (d *Drafts) keepFor(r drafts.Record) time.Duration {
	switch {
	case r.Mismatch || r.Key != "":
		return 0
	case r.Claimed:
		return d.ClaimedFor
	default:
		return d.UnclaimedFor
	}
}

// Put seals r and writes it, replacing what was there.
func (d *Drafts) Put(ctx context.Context, tenant, draft string, r drafts.Record) error {
	if tenant == "" || draft == "" {
		return errors.New("dynamodb: a draft record needs its tenant and draft")
	}
	plain, err := json.Marshal(r)
	if err != nil {
		return err
	}
	out, err := d.kms.Encrypt(ctx, &kms.EncryptInput{KeyId: &d.keyID, Plaintext: plain, EncryptionContext: draftContext(tenant, draft)})
	if err != nil {
		return fmt.Errorf("dynamodb: sealing the draft's record: %w", kmsErr(err))
	}
	item := pkey(draftsPK(tenant), draft)
	item["sealed"] = &types.AttributeValueMemberB{Value: out.CiphertextBlob}
	if keep := d.keepFor(r); keep > 0 {
		until := d.now().Add(keep)
		item["until"] = n(until.Unix())
		item["ttl"] = ttl(until)
	}
	if _, err := d.db.PutItem(ctx, &ddb.PutItemInput{TableName: &d.table, Item: item}); err != nil {
		return fmt.Errorf("dynamodb: writing the draft's record: %w", err)
	}
	return nil
}

// Get opens the draft's record; drafts.ErrNotFound if this tenant has none,
// and drafts.ErrGone, which is ErrNotFound too, if its time is up.
func (d *Drafts) Get(ctx context.Context, tenant, draft string) (drafts.Record, error) {
	if tenant == "" || draft == "" {
		return drafts.Record{}, drafts.ErrNotFound
	}
	out, err := d.db.GetItem(ctx, &ddb.GetItemInput{TableName: &d.table, Key: pkey(draftsPK(tenant), draft), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return drafts.Record{}, fmt.Errorf("dynamodb: reading the draft's record: %w", err)
	}
	if out.Item == nil {
		return drafts.Record{}, drafts.ErrNotFound
	}
	if until := getN(out.Item, "until"); until != 0 && !d.now().Before(time.Unix(until, 0)) {
		return drafts.Record{}, drafts.ErrGone
	}
	sealed, _ := out.Item["sealed"].(*types.AttributeValueMemberB)
	if sealed == nil || len(sealed.Value) == 0 {
		return drafts.Record{}, errors.New("dynamodb: the draft's record is not sealed")
	}
	opened, err := d.kms.Decrypt(ctx, &kms.DecryptInput{KeyId: &d.keyID, CiphertextBlob: sealed.Value, EncryptionContext: draftContext(tenant, draft)})
	if err != nil {
		return drafts.Record{}, fmt.Errorf("dynamodb: opening the draft's record: %w", kmsErr(err))
	}
	var r drafts.Record
	if err := json.Unmarshal(opened.Plaintext, &r); err != nil {
		return drafts.Record{}, errors.New("dynamodb: the draft's record is not a record")
	}
	return r, nil
}

// List opens each of the tenant's draft records, by draft, leaving out any
// past its "until". Each is opened under its own tenant and draft, as Get
// does, and before its time is read, so a record moved under this tenant
// fails the list, expired or not.
func (d *Drafts) List(ctx context.Context, tenant string) (map[string]drafts.Record, error) {
	if tenant == "" {
		return nil, errors.New("dynamodb: listing drafts needs a tenant")
	}
	in := &ddb.QueryInput{
		TableName:                 &d.table,
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(draftsPK(tenant))},
		ConsistentRead:            aws.Bool(true),
	}
	out := map[string]drafts.Record{}
	for {
		page, err := d.db.Query(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("dynamodb: listing the tenant's drafts: %w", err)
		}
		for _, it := range page.Items {
			draft := getS(it, "sk")
			sealed, _ := it["sealed"].(*types.AttributeValueMemberB)
			if draft == "" || sealed == nil || len(sealed.Value) == 0 {
				return nil, errors.New("dynamodb: a draft's record is not sealed")
			}
			opened, err := d.kms.Decrypt(ctx, &kms.DecryptInput{KeyId: &d.keyID, CiphertextBlob: sealed.Value, EncryptionContext: draftContext(tenant, draft)})
			if err != nil {
				return nil, fmt.Errorf("dynamodb: opening a draft's record: %w", kmsErr(err))
			}
			var r drafts.Record
			if err := json.Unmarshal(opened.Plaintext, &r); err != nil {
				return nil, errors.New("dynamodb: a draft's record is not a record")
			}
			// Left out past its time only once it's opened: a row that doesn't
			// open fails the list, expired or not.
			if until := getN(it, "until"); until != 0 && !d.now().Before(time.Unix(until, 0)) {
				continue
			}
			out[draft] = r
		}
		if len(page.LastEvaluatedKey) == 0 {
			return out, nil
		}
		in.ExclusiveStartKey = page.LastEvaluatedKey
	}
}

// Erase deletes every draft record of the tenant, when its account is deleted.
func (d *Drafts) Erase(ctx context.Context, tenant string) error {
	if tenant == "" {
		return errors.New("dynamodb: erasing drafts needs a tenant")
	}
	in := &ddb.QueryInput{
		TableName:                 &d.table,
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(draftsPK(tenant))},
		ProjectionExpression:      aws.String("pk, sk"),
	}
	for {
		page, err := d.db.Query(ctx, in)
		if err != nil {
			return fmt.Errorf("dynamodb: listing the tenant's drafts: %w", err)
		}
		for _, it := range page.Items {
			if _, err := d.db.DeleteItem(ctx, &ddb.DeleteItemInput{TableName: &d.table, Key: pkey(getS(it, "pk"), getS(it, "sk"))}); err != nil {
				return fmt.Errorf("dynamodb: erasing a draft: %w", err)
			}
		}
		if len(page.LastEvaluatedKey) == 0 {
			return nil
		}
		in.ExclusiveStartKey = page.LastEvaluatedKey
	}
}

// kmsErr is err with nothing of what was sealed in it: KMS's errors carry
// none, but only their type and message are kept, never a wrapped request.
func kmsErr(err error) error { return errors.New(err.Error()) }
