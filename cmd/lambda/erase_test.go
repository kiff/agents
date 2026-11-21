package main

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// eraseSecrets is Secrets Manager's list and delete, in memory.
type eraseSecrets struct{ names map[string]bool }

func (e *eraseSecrets) ListSecrets(_ context.Context, in *sm.ListSecretsInput, _ ...func(*sm.Options)) (*sm.ListSecretsOutput, error) {
	want := strings.TrimSuffix(in.Filters[0].Values[0], "/")
	out := &sm.ListSecretsOutput{}
	for n := range e.names {
		// As Secrets Manager's filter: more than the prefix matches.
		if strings.Contains(n, want) {
			out.SecretList = append(out.SecretList, types.SecretListEntry{Name: aws.String(n)})
		}
	}
	return out, nil
}

func (e *eraseSecrets) DeleteSecret(_ context.Context, in *sm.DeleteSecretInput, _ ...func(*sm.Options)) (*sm.DeleteSecretOutput, error) {
	delete(e.names, aws.ToString(in.SecretId))
	return &sm.DeleteSecretOutput{}, nil
}

func (e *eraseSecrets) RestoreSecret(context.Context, *sm.RestoreSecretInput, ...func(*sm.Options)) (*sm.RestoreSecretOutput, error) {
	return &sm.RestoreSecretOutput{}, nil
}

func (e *eraseSecrets) left() []string {
	var out []string
	for n := range e.names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// eraseTable is a table's items by tenant; it records the order of erases.
type eraseTable struct {
	name  string
	items map[string]int
	order *[]string
	fail  bool
}

func (t *eraseTable) Erase(_ context.Context, tenant string) error {
	*t.order = append(*t.order, t.name)
	if t.fail {
		return errors.New(t.name + ": throttled")
	}
	delete(t.items, tenant)
	return nil
}

func eraseFixture() (*eraseSecrets, *eraseTable, *eraseTable, *[]string) {
	order := &[]string{}
	s := &eraseSecrets{names: map[string]bool{
		"agents-sealing/t1": true, "agents-sealing/t10": true,
		"agents-drafts/t1/d": true, "agents-drafts/t10/d": true,
		"agents/t1/kiff/a": true, "agents/t1/kiff/b": true, "agents/t10/kiff/a": true, "agents/t2/kiff/a": true,
	}}
	drafts := &eraseTable{name: "drafts", items: map[string]int{"t1": 2, "t10": 1}, order: order}
	keys := &eraseTable{name: "keys", items: map[string]int{"t1": 2, "t10": 1, "t2": 1}, order: order}
	return s, drafts, keys, order
}

// With the tafs' keys in the table, an account's erase takes both stores of
// them: their old secrets and the table's items, the tenant's own only (not
// t10's, whose name starts the same), after its drafts and sealing key.
func TestAnAccountsEraseTakesBothKeyStores(t *testing.T) {
	s, drafts, keys, order := eraseFixture()
	erase := accountErase(s, "agents-sealing/", "agents-drafts/", "agents/", drafts, keys, nil)
	if err := erase(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	want := []string{"agents-drafts/t10/d", "agents-sealing/t10", "agents/t10/kiff/a", "agents/t2/kiff/a"}
	if got := s.left(); !slices.Equal(got, want) {
		t.Fatalf("secrets left %v, want %v", got, want)
	}
	if _, ok := keys.items["t1"]; ok || keys.items["t10"] != 1 || keys.items["t2"] != 1 {
		t.Fatalf("keys left %v", keys.items)
	}
	if _, ok := drafts.items["t1"]; ok || drafts.items["t10"] != 1 {
		t.Fatalf("drafts left %v", drafts.items)
	}
	if !slices.Equal(*order, []string{"drafts", "keys"}) {
		t.Fatalf("order %v", *order)
	}
}

// Without the keys in the table, their secrets are tafs cloud's: the erase
// leaves them, and touches no keys table.
func TestWithoutTheKeysStoreTheEraseLeavesTheirSecrets(t *testing.T) {
	s, drafts, _, order := eraseFixture()
	if err := accountErase(s, "agents-sealing/", "agents-drafts/", "agents/", drafts, nil, nil)(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"agents/t1/kiff/a", "agents/t1/kiff/b"} {
		if !s.names[n] {
			t.Fatalf("%s was erased", n)
		}
	}
	if !slices.Equal(*order, []string{"drafts"}) {
		t.Fatalf("order %v", *order)
	}
}

// The erase fails closed: a step that fails stops it with the error, and
// the keys in the table never go without their old secrets' prefix.
func TestAnAccountsEraseFailsClosed(t *testing.T) {
	s, drafts, keys, order := eraseFixture()
	drafts.fail = true
	if err := accountErase(s, "agents-sealing/", "agents-drafts/", "agents/", drafts, keys, nil)(ctx, "t1"); err == nil {
		t.Fatal("a failed step answered done")
	}
	if !slices.Equal(*order, []string{"drafts"}) || !s.names["agents/t1/kiff/a"] {
		t.Fatalf("went on after a failure: %v, secrets %v", *order, s.left())
	}
	s, drafts, keys, order = eraseFixture()
	if err := accountErase(s, "", "", "", drafts, keys, nil)(ctx, "t1"); err == nil {
		t.Fatal("the table's keys were erased with no prefix for their old secrets")
	}
	if _, ok := keys.items["t1"]; !ok || slices.Contains(*order, "keys") {
		t.Fatalf("the keys table was erased: %v", *order)
	}
	if accountErase(s, "", "", "", nil, nil, nil) != nil {
		t.Fatal("nothing to erase made an erase")
	}
}

// With the sealing keys in the table, an account's erase takes its old
// sealing secret first, then the table's, so the key never moves back in
// between; and never the table's without the old secrets' prefix.
func TestAnAccountsEraseTakesTheSealingKeyFromBothStores(t *testing.T) {
	s, drafts, keys, order := eraseFixture()
	sealing := &eraseTable{name: "sealing", items: map[string]int{"t1": 1, "t10": 1}, order: order}
	if err := accountErase(s, "agents-sealing/", "agents-drafts/", "agents/", drafts, keys, sealing)(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if s.names["agents-sealing/t1"] || !s.names["agents-sealing/t10"] {
		t.Fatalf("sealing secrets left %v", s.left())
	}
	if _, ok := sealing.items["t1"]; ok || sealing.items["t10"] != 1 {
		t.Fatalf("sealing keys left %v", sealing.items)
	}
	if !slices.Equal(*order, []string{"drafts", "keys", "sealing"}) {
		t.Fatalf("order %v", *order)
	}

	s, drafts, _, order = eraseFixture()
	sealing = &eraseTable{name: "sealing", items: map[string]int{"t1": 1}, order: order}
	if err := accountErase(s, "", "agents-drafts/", "", drafts, nil, sealing)(ctx, "t1"); err == nil {
		t.Fatal("the table's sealing key was erased with no prefix for its old secret")
	}
	if _, ok := sealing.items["t1"]; !ok {
		t.Fatalf("the sealing key was erased: %v", *order)
	}
}
