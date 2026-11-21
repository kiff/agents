package secretsmanager_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/kiff/agents/registry/secretsmanager"
)

// A tenant's sealing key is made once, 32 random bytes, then read back the
// same; each tenant has its own.
func TestSealingKeysAreMadeOncePerTenant(t *testing.T) {
	f := newFake()
	keys := secretsmanager.NewSealingKeys(writable{f}, "agents-sealing/")
	k1, err := keys.Key(ctx, "t1")
	if err != nil || len(k1) != 32 {
		t.Fatalf("key %d bytes, %v", len(k1), err)
	}
	again, err := keys.Key(ctx, "t1")
	if err != nil || !bytes.Equal(again, k1) {
		t.Fatalf("read back %x, %v; want the key made first", again, err)
	}
	k2, _ := keys.Key(ctx, "t2")
	if bytes.Equal(k1, k2) {
		t.Fatal("two tenants share a key")
	}
	if _, ok := f.secrets["agents-sealing/t1"]; !ok {
		t.Fatalf("the key isn't under the prefix: %v", f.asked)
	}
	if _, err := keys.Key(ctx, "t1/../t2"); err == nil {
		t.Fatal("a tenant that isn't one segment named a key")
	}
}

// Existing reads a tenant's key and never makes one: what a move to the
// table reads, so a tenant with none is told so and nothing is written.
func TestAnExistingSealingKeyIsReadNeverMade(t *testing.T) {
	f := newFake()
	keys := secretsmanager.NewSealingKeys(writable{f}, "agents-sealing/")
	made, err := keys.Key(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := keys.Existing(ctx, "t1"); err != nil || !bytes.Equal(got, made) {
		t.Fatalf("t1: %x %v", got, err)
	}
	if _, err := keys.Existing(ctx, "t2"); !errors.Is(err, secretsmanager.ErrNotFound) {
		t.Fatalf("t2: %v", err)
	}
	if _, ok := f.secrets["agents-sealing/t2"]; ok {
		t.Fatal("reading a missing key made one")
	}
	if _, err := keys.Existing(ctx, "t1/../t2"); err == nil {
		t.Fatal("a tenant that isn't one segment named a key")
	}
}
