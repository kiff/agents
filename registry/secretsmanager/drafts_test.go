package secretsmanager_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/kiff/agents/drafts"
	"github.com/kiff/agents/registry/secretsmanager"
)

// writable is the fake that also creates and updates secrets, as Secrets
// Manager does: create refuses an existing name, put refuses a missing one.
type writable struct{ *fake }

func (w writable) CreateSecret(_ context.Context, in *sm.CreateSecretInput, _ ...func(*sm.Options)) (*sm.CreateSecretOutput, error) {
	name := aws.ToString(in.Name)
	if _, ok := w.secrets[name]; ok {
		return nil, &types.ResourceExistsException{Message: aws.String("exists")}
	}
	w.secrets[name] = secret(name, aws.ToString(in.SecretString))
	return &sm.CreateSecretOutput{Name: in.Name}, nil
}

func (w writable) PutSecretValue(_ context.Context, in *sm.PutSecretValueInput, _ ...func(*sm.Options)) (*sm.PutSecretValueOutput, error) {
	name := aws.ToString(in.SecretId)
	if _, ok := w.secrets[name]; !ok {
		return nil, &types.ResourceNotFoundException{Message: aws.String("missing")}
	}
	w.secrets[name] = secret(name, aws.ToString(in.SecretString))
	return &sm.PutSecretValueOutput{}, nil
}

// A draft's record lives under its own prefix, by tenant: created, then
// replaced, and never read for another tenant.
func TestDraftRecords(t *testing.T) {
	w := writable{newFake()}
	d := secretsmanager.NewDrafts(w, "agent-drafts/")
	if err := d.Put(ctx, "t1", "drf_1", drafts.Record{AgentID: "agt", Secret: "kiff_claim_x"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.secrets["agent-drafts/t1/drf_1"]; !ok {
		t.Fatalf("not under the prefix: %v", w.secrets)
	}
	if err := d.Put(ctx, "t1", "drf_1", drafts.Record{AgentID: "agt", KeyRef: "kiff/taf-1", Claimed: true}); err != nil {
		t.Fatal(err)
	}
	r, err := d.Get(ctx, "t1", "drf_1")
	if err != nil || r.Secret != "" || !r.Claimed {
		t.Fatalf("after the claim: %+v %v", r, err)
	}
	for _, c := range [][2]string{{"t2", "drf_1"}, {"t1", "../t2/drf_1"}, {"t1/x", "drf_1"}} {
		if _, err := d.Get(ctx, c[0], c[1]); !errors.Is(err, drafts.ErrNotFound) {
			t.Errorf("%v: %v", c, err)
		}
	}
}

// A claimed key is created where Secrets reads it, and never replaces one.
func TestKeysAreCreatedNeverReplaced(t *testing.T) {
	w := writable{newFake()}
	k := secretsmanager.NewKeys(secretsmanager.New(w, "agents/"), w)
	if err := k.Create(ctx, "t1", "kiff/payouts", "kiff_key_new"); !errors.Is(err, drafts.ErrTaken) {
		t.Fatalf("over an existing key: %v", err)
	}
	if got, _, _ := k.Key(ctx, "t1", "kiff/payouts"); got != "key-t1" {
		t.Fatalf("the existing key changed: %q", got)
	}
	if _, found, err := k.Key(ctx, "t1", "kiff/taf-9"); found || err != nil {
		t.Fatalf("a free ref: %v %v", found, err)
	}
	if err := k.Create(ctx, "t1", "kiff/taf-9", "kiff_key_new"); err != nil {
		t.Fatal(err)
	}
	if got, found, _ := k.Key(ctx, "t1", "kiff/taf-9"); !found || got != "kiff_key_new" {
		t.Fatalf("stored %q %v", got, found)
	}
	if err := k.Create(ctx, "t1", "../t2/kiff/x", "k"); err == nil {
		t.Fatal("a ref outside the tenant was written")
	}
}
