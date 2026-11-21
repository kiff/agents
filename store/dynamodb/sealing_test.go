package dynamodb_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kiff/agents/store/dynamodb"
)

func sealingStore(t *testing.T) (*dynamodb.SealingKeys, *fakeKMS, func(tenant string) []byte) {
	t.Helper()
	db := local(t)
	name := table(t, db)
	k := &fakeKMS{}
	raw := func(tenant string) []byte {
		b, _ := json.Marshal(rawItem(t, db, name, tenant, "sealing"))
		return b
	}
	return dynamodb.NewSealingKeys(db, name, k, "alias/keys"), k, raw
}

func oldKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

// A tenant with an old sealing key keeps those same bytes: moved to the
// table on the first read, sealed there, and read from the table after,
// without asking the old store again.
func TestAnOldSealingKeyMovesItsSameBytes(t *testing.T) {
	s, _, raw := sealingStore(t)
	asked := 0
	s.Old = func(_ context.Context, tenant string) ([]byte, error) {
		asked++
		if tenant == "t1" {
			return oldKey(7), nil
		}
		return nil, dynamodb.ErrSealingKeyNotFound
	}
	for range 2 {
		got, err := s.Key(ctx, "t1")
		if err != nil || !bytes.Equal(got, oldKey(7)) {
			t.Fatalf("t1's key: %x %v", got, err)
		}
	}
	if asked != 1 {
		t.Errorf("the old store was asked %d times, want once", asked)
	}
	if b := raw("t1"); len(b) < 3 || bytes.Contains(b, oldKey(7)) {
		t.Errorf("the key isn't kept sealed: %s", b)
	}
}

// A tenant with no key anywhere gets one made, 32 bytes, kept; another
// tenant's is its own.
func TestANewTenantsSealingKeyIsMadeOnce(t *testing.T) {
	s, _, _ := sealingStore(t)
	s.Old = func(context.Context, string) ([]byte, error) { return nil, dynamodb.ErrSealingKeyNotFound }
	a, err := s.Key(ctx, "t1")
	if err != nil || len(a) != 32 {
		t.Fatalf("made: %x %v", a, err)
	}
	if again, _ := s.Key(ctx, "t1"); !bytes.Equal(again, a) {
		t.Fatal("the key changed on its second read")
	}
	if b, _ := s.Key(ctx, "t2"); bytes.Equal(b, a) {
		t.Fatal("two tenants share a key")
	}
}

// Never a new key while an old one may exist: an old store that can't be
// read fails the call, writes nothing, and the next read moves the old key.
func TestAnUnreadOldSealingKeyIsNeverReplaced(t *testing.T) {
	s, _, _ := sealingStore(t)
	down := true
	s.Old = func(context.Context, string) ([]byte, error) {
		if down {
			return nil, errors.New("secretsmanager: throttled")
		}
		return oldKey(9), nil
	}
	if _, err := s.Key(ctx, "t1"); err == nil {
		t.Fatal("answered a key while the old one couldn't be read")
	}
	down = false
	if got, err := s.Key(ctx, "t1"); err != nil || !bytes.Equal(got, oldKey(9)) {
		t.Fatalf("after: %x %v", got, err)
	}
	s.Old = func(context.Context, string) ([]byte, error) { return []byte("short"), nil }
	if _, err := s.Key(ctx, "t2"); err == nil {
		t.Fatal("moved an old key that isn't 32 bytes")
	}
}

// Without KMS nothing is read or written; a key sealed for one tenant
// doesn't open for another; an erase leaves the next read a new key.
func TestSealingKeysFailClosedAndErase(t *testing.T) {
	s, k, _ := sealingStore(t)
	s.Old = func(context.Context, string) ([]byte, error) { return nil, dynamodb.ErrSealingKeyNotFound }
	k.down = true
	if _, err := s.Key(ctx, "t1"); err == nil {
		t.Fatal("a key without KMS")
	}
	k.down = false
	first, err := s.Key(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Erase(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if again, err := s.Key(ctx, "t1"); err != nil || bytes.Equal(again, first) {
		t.Fatalf("after the erase: %v, same key %v", err, bytes.Equal(again, first))
	}
	if _, err := s.Key(ctx, "t1|x"); err == nil {
		t.Fatal("a tenant that isn't one segment")
	}
}
