package secretsmanager_test

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/kiff/agents/registry/secretsmanager"
)

// erasable is the fake that also lists and deletes, as Secrets Manager
// does: the name filter matches more than a prefix, a page holds one secret,
// and deleting a missing secret is not found.
type erasable struct {
	*fake
	deleted []string
	forced  bool
}

func (e *erasable) ListSecrets(_ context.Context, in *sm.ListSecretsInput, _ ...func(*sm.Options)) (*sm.ListSecretsOutput, error) {
	want := in.Filters[0].Values[0]
	var names []string
	for n := range e.secrets {
		// Secrets Manager's name filter also matches words inside a name, so
		// the fake matches anywhere: Erase must check the prefix itself.
		if strings.Contains(n, strings.TrimSuffix(want, "/")) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	i := 0
	if in.NextToken != nil {
		i = int(aws.ToString(in.NextToken)[0] - '0')
	}
	out := &sm.ListSecretsOutput{}
	if i < len(names) {
		out.SecretList = []types.SecretListEntry{{Name: aws.String(names[i])}}
	}
	if i+1 < len(names) {
		out.NextToken = aws.String(string(rune('0' + i + 1)))
	}
	return out, nil
}

func (e *erasable) DeleteSecret(_ context.Context, in *sm.DeleteSecretInput, _ ...func(*sm.Options)) (*sm.DeleteSecretOutput, error) {
	id := aws.ToString(in.SecretId)
	e.forced = aws.ToBool(in.ForceDeleteWithoutRecovery)
	if _, ok := e.secrets[id]; !ok {
		return nil, &types.ResourceNotFoundException{Message: aws.String("missing")}
	}
	delete(e.secrets, id)
	e.deleted = append(e.deleted, id)
	return &sm.DeleteSecretOutput{}, nil
}

// Deleting an account erases its sealing key and its drafts' records at
// once, every page of them, and nothing of another tenant's, nor the tafs'
// keys, which are tafs cloud's (kiff/tafs#185). Asked again, it's done.
func TestEraseDeletesOnlyTheTenantsOwn(t *testing.T) {
	e := &erasable{fake: &fake{secrets: map[string]*sm.GetSecretValueOutput{
		"agents-sealing/t1":   secret("agents-sealing/t1", "k1"),
		"agents-sealing/t10":  secret("agents-sealing/t10", "k10"),
		"agents-drafts/t1/d1": secret("agents-drafts/t1/d1", "r"),
		"agents-drafts/t1/d2": secret("agents-drafts/t1/d2", "r"),
		"agents-drafts/t10/d": secret("agents-drafts/t10/d", "r"),
		"agents-drafts/t2/d1": secret("agents-drafts/t2/d1", "r"),
		"agents/t1/kiff/taf":  secret("agents/t1/kiff/taf", "key"),
	}}}
	er := secretsmanager.NewEraser(e, "agents-sealing/", "agents-drafts/")
	if err := er.Erase(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	slices.Sort(e.deleted)
	if want := []string{"agents-drafts/t1/d1", "agents-drafts/t1/d2", "agents-sealing/t1"}; !slices.Equal(e.deleted, want) || !e.forced {
		t.Fatalf("deleted %v (forced %v), want %v at once", e.deleted, e.forced, want)
	}
	for _, kept := range []string{"agents-sealing/t10", "agents-drafts/t10/d", "agents-drafts/t2/d1", "agents/t1/kiff/taf"} {
		if _, ok := e.secrets[kept]; !ok {
			t.Errorf("%s was deleted", kept)
		}
	}
	if err := er.Erase(ctx, "t1"); err != nil {
		t.Fatalf("erasing again: %v", err)
	}
	if err := er.Erase(ctx, "a/b"); err == nil {
		t.Fatal("a tenant that isn't one segment was erased")
	}
}
