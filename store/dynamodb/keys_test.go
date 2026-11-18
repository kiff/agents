package dynamodb_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/kiff/agents/keys"
	"github.com/kiff/agents/store/dynamodb"
)

func keysStore(t *testing.T) (*dynamodb.Keys, *ddb.Client, string, *fakeKMS, *time.Time) {
	t.Helper()
	db := local(t)
	name := table(t, db)
	k := &fakeKMS{}
	now := draftT0
	return dynamodb.NewKeys(db, name, k, "alias/keys").WithClock(func() time.Time { return now }), db, name, k, &now
}

// A key is stored sealed, never in clear, and reads back whole, for its own
// tenant only.
func TestATafKeyIsSealed(t *testing.T) {
	k, db, name, _, _ := keysStore(t)
	if err := k.Put(ctx, "t1", "kiff/taf_1", "kiff_key_live_x"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rawItem(t, db, name, "t1", "kiff/taf_1"))
	if bytes.Contains(raw, []byte("kiff_key_live_x")) {
		t.Fatalf("the key is in clear in the item: %s", raw)
	}
	if got, err := k.Get(ctx, "t1", "kiff/taf_1"); err != nil || got != "kiff_key_live_x" {
		t.Fatalf("read back %q %v", got, err)
	}
	if _, err := k.Get(ctx, "t2", "kiff/taf_1"); !errors.Is(err, keys.ErrNotFound) || errors.Is(err, keys.ErrRemoved) {
		t.Fatalf("another tenant: %v", err)
	}
}

// Create is create-only: a second create is refused and the first key
// stays; a removed key's mark refuses it too. Put replaces either.
func TestATafKeyCreateIsCreateOnly(t *testing.T) {
	k, _, _, _, _ := keysStore(t)
	if err := k.Create(ctx, "t1", "kiff/taf_1", "kiff_key_first"); err != nil {
		t.Fatal(err)
	}
	if err := k.Create(ctx, "t1", "kiff/taf_1", "kiff_key_second"); !errors.Is(err, keys.ErrTaken) {
		t.Fatalf("second create: %v", err)
	}
	if got, _ := k.Get(ctx, "t1", "kiff/taf_1"); got != "kiff_key_first" {
		t.Fatalf("the first key was replaced: %q", got)
	}
	if err := k.Remove(ctx, "t1", "kiff/taf_1", false); err != nil {
		t.Fatal(err)
	}
	if err := k.Create(ctx, "t1", "kiff/taf_1", "kiff_key_again"); !errors.Is(err, keys.ErrTaken) {
		t.Fatalf("create over a removed key's mark: %v", err)
	}
	if err := k.Put(ctx, "t1", "kiff/taf_1", "kiff_key_put"); err != nil {
		t.Fatal(err)
	}
	if got, err := k.Get(ctx, "t1", "kiff/taf_1"); err != nil || got != "kiff_key_put" {
		t.Fatalf("after a put: %q %v", got, err)
	}
}

// A removed key's mark: kept with no expiry while its old secret may be
// there; a settled one for a day, read as removed until then.
func TestARemovedTafKeysMark(t *testing.T) {
	k, db, name, _, now := keysStore(t)
	for _, c := range []struct {
		ref     string
		settled bool
	}{{"kiff/kept", false}, {"kiff/settled", true}} {
		if err := k.Put(ctx, "t1", c.ref, "kiff_key_x"); err != nil {
			t.Fatal(err)
		}
		if err := k.Remove(ctx, "t1", c.ref, c.settled); err != nil {
			t.Fatal(err)
		}
		it := rawItem(t, db, name, "t1", c.ref)
		if _, sealed := it["sealed"]; sealed {
			t.Fatalf("%s: the key stayed in the mark", c.ref)
		}
		if _, hasTTL := it["ttl"]; hasTTL != c.settled {
			t.Fatalf("%s: ttl %v", c.ref, it["ttl"])
		}
		if _, err := k.Get(ctx, "t1", c.ref); !errors.Is(err, keys.ErrRemoved) {
			t.Fatalf("%s: %v", c.ref, err)
		}
	}
	*now = draftT0.Add(dynamodb.KeyRemovedFor)
	if _, err := k.Get(ctx, "t1", "kiff/settled"); !errors.Is(err, keys.ErrNotFound) || errors.Is(err, keys.ErrRemoved) {
		t.Fatalf("a settled mark past its time: %v", err)
	}
	if _, err := k.Get(ctx, "t1", "kiff/kept"); !errors.Is(err, keys.ErrRemoved) {
		t.Fatalf("a kept mark later: %v", err)
	}
}

// kiff-qa's pin 3: the context is {purpose, tenant, ref} from the call. A
// sealed key copied to another tenant's or another reference's item
// doesn't open there.
func TestATafKeyMovedElsewhereDoesNotOpen(t *testing.T) {
	k, db, name, _, _ := keysStore(t)
	if err := k.Put(ctx, "t1", "kiff/taf_1", "kiff_key_t1"); err != nil {
		t.Fatal(err)
	}
	for _, to := range [][2]string{{"t2", "kiff/taf_1"}, {"t1", "kiff/taf_2"}} {
		if err := k.Put(ctx, to[0], to[1], "kiff_key_other"); err != nil {
			t.Fatal(err)
		}
		moved := rawItem(t, db, name, "t1", "kiff/taf_1")
		target := rawItem(t, db, name, to[0], to[1])
		moved["pk"], moved["sk"] = target["pk"], target["sk"]
		if _, err := db.PutItem(ctx, &ddb.PutItemInput{TableName: &name, Item: moved}); err != nil {
			t.Fatal(err)
		}
		got, err := k.Get(ctx, to[0], to[1])
		if err == nil || errors.Is(err, keys.ErrNotFound) || got == "kiff_key_t1" {
			t.Fatalf("t1's key opened as %v: %q %v", to, got, err)
		}
	}
}

// Without KMS nothing is written or read, and no error carries the key.
func TestTafKeysFailClosed(t *testing.T) {
	k, db, name, kms, _ := keysStore(t)
	if err := k.Put(ctx, "t1", "kiff/taf_1", "kiff_key_a"); err != nil {
		t.Fatal(err)
	}
	kms.down = true
	for _, err := range []error{k.Put(ctx, "t1", "kiff/taf_2", "kiff_key_secret_b"), k.Create(ctx, "t1", "kiff/taf_3", "kiff_key_secret_b")} {
		if err == nil || strings.Contains(err.Error(), "kiff_key_secret_b") {
			t.Fatalf("written without KMS, or the error carries the key: %v", err)
		}
	}
	if rawItem(t, db, name, "t1", "kiff/taf_2") != nil || rawItem(t, db, name, "t1", "kiff/taf_3") != nil {
		t.Fatal("an item was written without KMS")
	}
	if _, err := k.Get(ctx, "t1", "kiff/taf_1"); err == nil || errors.Is(err, keys.ErrNotFound) {
		t.Fatalf("read without KMS: %v", err)
	}
}

// Erasing a tenant's account takes its keys and marks, and only its own.
func TestTafKeysErase(t *testing.T) {
	k, _, _, _, _ := keysStore(t)
	for _, c := range [][2]string{{"t1", "kiff/a"}, {"t1", "kiff/b"}, {"t10", "kiff/a"}, {"t2", "kiff/a"}} {
		if err := k.Put(ctx, c[0], c[1], "kiff_key"); err != nil {
			t.Fatal(err)
		}
	}
	if err := k.Remove(ctx, "t1", "kiff/b", false); err != nil {
		t.Fatal(err)
	}
	if err := k.Erase(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	// Not rawItem: "t1" is in t10's key too. Gone, the mark included.
	for _, ref := range []string{"kiff/a", "kiff/b"} {
		if _, err := k.Get(ctx, "t1", ref); !errors.Is(err, keys.ErrNotFound) || errors.Is(err, keys.ErrRemoved) {
			t.Fatalf("t1 %s kept: %v", ref, err)
		}
	}
	for _, tn := range []string{"t10", "t2"} {
		if _, err := k.Get(ctx, tn, "kiff/a"); err != nil {
			t.Fatalf("%s erased: %v", tn, err)
		}
	}
}
