package dynamodb_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"

	"github.com/kiff/agents/drafts"
	"github.com/kiff/agents/store/dynamodb"
)

// fakeKMS seals as KMS does for these tests: what it returns can't be read
// without it, and opens only under the same key and encryption context.
type fakeKMS struct {
	down  bool
	calls int
}

func ecDigest(key string, ec map[string]string) []byte {
	names := make([]string, 0, len(ec))
	for k := range ec {
		names = append(names, k)
	}
	sort.Strings(names)
	h := sha256.New()
	h.Write([]byte(key))
	for _, k := range names {
		h.Write([]byte("\x00" + k + "=" + ec[k]))
	}
	return h.Sum(nil)
}

func (f *fakeKMS) Encrypt(_ context.Context, in *kms.EncryptInput, _ ...func(*kms.Options)) (*kms.EncryptOutput, error) {
	f.calls++
	if f.down {
		return nil, errors.New("kms: unavailable")
	}
	tag := ecDigest(aws.ToString(in.KeyId), in.EncryptionContext)
	sealed := make([]byte, len(in.Plaintext))
	for i, b := range in.Plaintext {
		sealed[i] = b ^ tag[i%len(tag)]
	}
	return &kms.EncryptOutput{CiphertextBlob: append(tag, sealed...)}, nil
}

func (f *fakeKMS) Decrypt(_ context.Context, in *kms.DecryptInput, _ ...func(*kms.Options)) (*kms.DecryptOutput, error) {
	f.calls++
	if f.down {
		return nil, errors.New("kms: unavailable")
	}
	tag := ecDigest(aws.ToString(in.KeyId), in.EncryptionContext)
	if len(in.CiphertextBlob) < len(tag) || !bytes.Equal(in.CiphertextBlob[:len(tag)], tag) {
		return nil, errors.New("kms: InvalidCiphertextException")
	}
	sealed := in.CiphertextBlob[len(tag):]
	plain := make([]byte, len(sealed))
	for i, b := range sealed {
		plain[i] = b ^ tag[i%len(tag)]
	}
	return &kms.DecryptOutput{Plaintext: plain}, nil
}

var draftT0 = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func draftsStore(t *testing.T) (*dynamodb.Drafts, *ddb.Client, string, *fakeKMS, *time.Time) {
	t.Helper()
	db := local(t)
	name := table(t, db)
	k := &fakeKMS{}
	now := draftT0
	return dynamodb.NewDrafts(db, name, k, "alias/drafts").WithClock(func() time.Time { return now }), db, name, k, &now
}

func rawItem(t *testing.T, db *ddb.Client, name, tenant, draft string) map[string]types.AttributeValue {
	t.Helper()
	out, err := db.Scan(context.Background(), &ddb.ScanInput{TableName: &name})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range out.Items {
		if sk, _ := it["sk"].(*types.AttributeValueMemberS); sk != nil && sk.Value == draft {
			// The tenant is the key's last part: "t1" must not find t10's.
			if pk, _ := it["pk"].(*types.AttributeValueMemberS); pk != nil && strings.HasSuffix(pk.Value, ":"+tenant) {
				return it
			}
		}
	}
	return nil
}

// A record is stored sealed: the claim secret isn't in the item, and it
// reads back whole.
func TestDraftRecordIsSealed(t *testing.T) {
	d, db, name, _, _ := draftsStore(t)
	rec := drafts.Record{AgentID: "agt_1", Secret: "kiff_claim_secret_x"}
	if err := d.Put(ctx, "t1", "drf_1", rec); err != nil {
		t.Fatal(err)
	}
	it := rawItem(t, db, name, "t1", "drf_1")
	if it == nil {
		t.Fatal("no item")
	}
	raw, _ := json.Marshal(it)
	if bytes.Contains(raw, []byte("kiff_claim_secret_x")) || bytes.Contains(raw, []byte("agt_1")) {
		t.Fatalf("the record is in clear in the item: %s", raw)
	}
	got, err := d.Get(ctx, "t1", "drf_1")
	if err != nil || got != rec {
		t.Fatalf("read back %+v %v", got, err)
	}
	if _, err := d.Get(ctx, "t2", "drf_1"); !errors.Is(err, drafts.ErrNotFound) {
		t.Fatalf("another tenant: %v", err)
	}
}

// How long a record is kept follows what it holds; one holding a live key
// is never given an expiry.
func TestDraftRecordKeptForWhatItHolds(t *testing.T) {
	d, db, name, _, _ := draftsStore(t)
	cases := []struct {
		draft string
		rec   drafts.Record
		keep  time.Duration
	}{
		{"unclaimed", drafts.Record{AgentID: "a", Secret: "s"}, 48 * time.Hour},
		{"claimed", drafts.Record{AgentID: "a", KeyRef: "kiff/taf", Claimed: true}, 14 * 24 * time.Hour},
		{"halfway", drafts.Record{AgentID: "a", Key: "kiff_key_live", KeyID: "kid"}, 0},
		{"mismatch", drafts.Record{AgentID: "a", Key: "kiff_key_other", KeyID: "kid", Mismatch: true}, 0},
	}
	for _, c := range cases {
		if err := d.Put(ctx, "t1", c.draft, c.rec); err != nil {
			t.Fatal(err)
		}
		it := rawItem(t, db, name, "t1", c.draft)
		ttlV, hasTTL := it["ttl"].(*types.AttributeValueMemberN)
		_, hasUntil := it["until"]
		if c.keep == 0 {
			if hasTTL || hasUntil {
				t.Fatalf("%s: a record holding a live key has an expiry: %v", c.draft, it)
			}
			continue
		}
		want := draftT0.Add(c.keep).Unix()
		if !hasTTL || ttlV.Value != strconv.FormatInt(want, 10) || !hasUntil {
			t.Fatalf("%s: ttl %v, want %d", c.draft, it["ttl"], want)
		}
	}
}

// DynamoDB removes an expired item late: a record past its time reads as
// gone while it's still in the table, and a second before it still reads.
func TestDraftRecordPastItsTimeIsGone(t *testing.T) {
	d, db, name, _, now := draftsStore(t)
	if err := d.Put(ctx, "t1", "drf_1", drafts.Record{AgentID: "a", Secret: "s"}); err != nil {
		t.Fatal(err)
	}
	*now = draftT0.Add(48*time.Hour - time.Second)
	if _, err := d.Get(ctx, "t1", "drf_1"); err != nil {
		t.Fatalf("a second before its time: %v", err)
	}
	*now = draftT0.Add(48 * time.Hour)
	if _, err := d.Get(ctx, "t1", "drf_1"); !errors.Is(err, drafts.ErrNotFound) || !errors.Is(err, drafts.ErrGone) {
		t.Fatalf("at its time: %v", err)
	}
	if rawItem(t, db, name, "t1", "drf_1") == nil {
		t.Fatal("the test needs the item still in the table")
	}
}

// The encryption context comes from the call: a sealed record moved to
// another tenant's key doesn't open there.
func TestDraftRecordMovedToAnotherTenantDoesNotOpen(t *testing.T) {
	d, db, name, _, _ := draftsStore(t)
	if err := d.Put(ctx, "t1", "drf_1", drafts.Record{AgentID: "a", Secret: "s"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Put(ctx, "t2", "drf_1", drafts.Record{AgentID: "b", Secret: "other"}); err != nil {
		t.Fatal(err)
	}
	moved := rawItem(t, db, name, "t1", "drf_1")
	target := rawItem(t, db, name, "t2", "drf_1")
	moved["pk"], moved["sk"] = target["pk"], target["sk"]
	if _, err := db.PutItem(ctx, &ddb.PutItemInput{TableName: &name, Item: moved}); err != nil {
		t.Fatal(err)
	}
	r, err := d.Get(ctx, "t2", "drf_1")
	if err == nil || errors.Is(err, drafts.ErrNotFound) || r.Secret == "s" {
		t.Fatalf("t1's record opened as t2's: %+v %v", r, err)
	}
}

// Without KMS nothing is written or read, and no error carries the record.
func TestDraftRecordFailsClosed(t *testing.T) {
	d, db, name, k, _ := draftsStore(t)
	if err := d.Put(ctx, "t1", "drf_1", drafts.Record{AgentID: "a", Secret: "s"}); err != nil {
		t.Fatal(err)
	}
	k.down = true
	err := d.Put(ctx, "t1", "drf_2", drafts.Record{AgentID: "a", Secret: "kiff_claim_secret_y", Key: "kiff_key_z"})
	if err == nil {
		t.Fatal("a record was written without KMS")
	}
	if rawItem(t, db, name, "t1", "drf_2") != nil {
		t.Fatal("an item was written without KMS")
	}
	if strings.Contains(err.Error(), "kiff_claim_secret_y") || strings.Contains(err.Error(), "kiff_key_z") {
		t.Fatalf("the error carries the record: %v", err)
	}
	if _, err := d.Get(ctx, "t1", "drf_1"); err == nil || errors.Is(err, drafts.ErrNotFound) {
		t.Fatalf("read without KMS: %v", err)
	}
}

// Erasing a tenant's account takes its drafts and only its own.
func TestDraftRecordsErase(t *testing.T) {
	d, _, _, _, _ := draftsStore(t)
	for _, c := range [][2]string{{"t1", "a"}, {"t1", "b"}, {"t10", "a"}, {"t2", "a"}} {
		if err := d.Put(ctx, c[0], c[1], drafts.Record{AgentID: "x", Secret: "s"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Erase(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{{"t1", "a"}, {"t1", "b"}} {
		if _, err := d.Get(ctx, c[0], c[1]); !errors.Is(err, drafts.ErrNotFound) {
			t.Fatalf("%v kept: %v", c, err)
		}
	}
	for _, c := range [][2]string{{"t10", "a"}, {"t2", "a"}} {
		if _, err := d.Get(ctx, c[0], c[1]); err != nil {
			t.Fatalf("%v erased: %v", c, err)
		}
	}
}

// List opens the tenant's records and only its own, leaving out any past
// its time; one moved from another tenant doesn't open.
func TestDraftRecordsList(t *testing.T) {
	d, db, name, _, now := draftsStore(t)
	for _, c := range []struct {
		tenant, draft string
		r             drafts.Record
	}{
		{"t1", "drf_a", drafts.Record{AgentID: "a", Secret: "s", Label: "A", Requester: &drafts.Requester{Kind: "member", ID: "u1"}}},
		{"t1", "drf_b", drafts.Record{AgentID: "b", Claimed: true, KeyRef: "r"}},
		{"t10", "drf_c", drafts.Record{AgentID: "c", Claimed: true, KeyRef: "r10"}},
	} {
		if err := d.Put(ctx, c.tenant, c.draft, c.r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := d.List(ctx, "t1")
	if err != nil || len(got) != 2 || got["drf_a"].Label != "A" || got["drf_a"].Requester == nil || got["drf_a"].Requester.ID != "u1" || !got["drf_b"].Claimed {
		t.Fatalf("t1: %+v %v", got, err)
	}
	// Past drf_a's time (48 h unclaimed), within drf_b's (14 days claimed).
	*now = draftT0.Add(48 * time.Hour)
	if got, err := d.List(ctx, "t1"); err != nil || len(got) != 1 || got["drf_b"].AgentID != "b" {
		t.Fatalf("past drf_a's time: %+v %v", got, err)
	}
	// A record moved under t1 from t10 doesn't open: the list fails, never
	// shows it, whether the record is within its time (drf_c, claimed) or
	// past it (drf_d, unclaimed at the start).
	for _, from := range []string{"drf_c", "drf_d"} {
		if from == "drf_d" {
			*now = draftT0
			if err := d.Put(ctx, "t10", "drf_d", drafts.Record{AgentID: "d", Secret: "s"}); err != nil {
				t.Fatal(err)
			}
			*now = draftT0.Add(48 * time.Hour)
		}
		moved := rawItem(t, db, name, "t10", from)
		moved["pk"] = rawItem(t, db, name, "t1", "drf_b")["pk"]
		if _, err := db.PutItem(ctx, &ddb.PutItemInput{TableName: &name, Item: moved}); err != nil {
			t.Fatal(err)
		}
		if got, err := d.List(ctx, "t1"); err == nil || got != nil {
			t.Fatalf("%s moved under t1 was listed: %+v %v", from, got, err)
		}
		if _, err := db.DeleteItem(ctx, &ddb.DeleteItemInput{TableName: &name, Key: map[string]types.AttributeValue{"pk": moved["pk"], "sk": moved["sk"]}}); err != nil {
			t.Fatal(err)
		}
	}
}
