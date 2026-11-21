package dynamodb_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kiff/agents/store/dynamodb"
)

// A tenant's alert secret is kept sealed, never in clear, read back for its
// own tenant only; Put replaces it, Create never does, Remove and Erase
// leave a removed mark that only a Put replaces; without KMS nothing is read or written.
func TestAnAlertSecretIsSealedAndCreateOnlyOnAMove(t *testing.T) {
	db := local(t)
	name := table(t, db)
	k := &fakeKMS{}
	a := dynamodb.NewAlertSecrets(db, name, k, "alias/keys")

	if copied, err := a.Create(ctx, "t1", "whsec_old"); err != nil || !copied {
		t.Fatalf("create: %v %v", copied, err)
	}
	raw, _ := json.Marshal(rawItem(t, db, name, "t1", "alert"))
	if bytes.Contains(raw, []byte("whsec_old")) {
		t.Fatalf("the secret is in clear: %s", raw)
	}
	if err := a.Put(ctx, "t1", "whsec_new"); err != nil {
		t.Fatal(err)
	}
	if copied, err := a.Create(ctx, "t1", "whsec_old"); err != nil || copied {
		t.Fatalf("a move wrote over the API's secret: %v %v", copied, err)
	}
	if got, err := a.Get(ctx, "t1"); err != nil || got != "whsec_new" {
		t.Fatalf("read back %q %v", got, err)
	}
	if _, err := a.Get(ctx, "t2"); !errors.Is(err, dynamodb.ErrNoAlertSecret) {
		t.Fatalf("another tenant: %v", err)
	}
	k.down = true
	if _, err := a.Get(ctx, "t1"); err == nil || errors.Is(err, dynamodb.ErrNoAlertSecret) {
		t.Fatalf("KMS down read: %v", err)
	}
	if err := a.Put(ctx, "t1", "whsec_x"); err == nil {
		t.Fatal("KMS down wrote")
	}
	k.down = false
	if err := a.Remove(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Get(ctx, "t1"); !errors.Is(err, dynamodb.ErrAlertSecretRemoved) {
		t.Fatalf("after remove: %v", err)
	}
	// The mark holds: a move never writes over it, removing again keeps
	// it, and an account's erase leaves one too.
	if copied, err := a.Create(ctx, "t1", "whsec_old"); err != nil || copied {
		t.Fatalf("a move revived a removed secret: %v %v", copied, err)
	}
	if err := a.Remove(ctx, "t1"); err != nil {
		t.Fatalf("removing again: %v", err)
	}
	if err := a.Erase(ctx, "t3"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Get(ctx, "t3"); !errors.Is(err, dynamodb.ErrAlertSecretRemoved) {
		t.Fatalf("after erase: %v", err)
	}
	// A new secret replaces the mark.
	if err := a.Put(ctx, "t1", "whsec_again"); err != nil {
		t.Fatal(err)
	}
	if got, err := a.Get(ctx, "t1"); err != nil || got != "whsec_again" {
		t.Fatalf("after a new put: %q %v", got, err)
	}
	if err := a.Put(ctx, "t1|x", "whsec_x"); err == nil {
		t.Fatal("a tenant that isn't one segment")
	}
}
