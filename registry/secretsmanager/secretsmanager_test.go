package secretsmanager_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/kiff/agents/registry/secretsmanager"
)

var ctx = context.Background()

// fake is Secrets Manager as far as the adapter sees it: secrets by name.
type fake struct {
	secrets map[string]*sm.GetSecretValueOutput
	asked   []string
	fail    error
	// createFail, when set, is what CreateSecret answers (writable).
	createFail error
}

func (f *fake) GetSecretValue(_ context.Context, in *sm.GetSecretValueInput, _ ...func(*sm.Options)) (*sm.GetSecretValueOutput, error) {
	id := aws.ToString(in.SecretId)
	f.asked = append(f.asked, id)
	if f.fail != nil {
		return nil, f.fail
	}
	out, ok := f.secrets[id]
	if !ok {
		return nil, &types.ResourceNotFoundException{Message: aws.String("not found")}
	}
	return out, nil
}

func secret(name, value string) *sm.GetSecretValueOutput {
	return &sm.GetSecretValueOutput{Name: aws.String(name), SecretString: aws.String(value)}
}

func newFake() *fake {
	return &fake{secrets: map[string]*sm.GetSecretValueOutput{
		"agents/t1/kiff/payouts": secret("agents/t1/kiff/payouts", "key-t1\n"),
		"agents/t2/kiff/payouts": secret("agents/t2/kiff/payouts", "key-t2"),
	}}
}

// A reference resolves within its tenant: the same reference is another
// secret for another tenant.
func TestAKeyIsTheTenantsOwn(t *testing.T) {
	f := newFake()
	s := secretsmanager.New(f, "agents/")
	for tenant, want := range map[string]string{"t1": "key-t1", "t2": "key-t2"} {
		got, err := s.Key(ctx, tenant, "kiff/payouts")
		if err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", tenant, got, err, want)
		}
	}
	if _, err := s.Key(ctx, "t3", "kiff/payouts"); !errors.Is(err, secretsmanager.ErrNotFound) {
		t.Errorf("t3, which has no such secret: err = %v, want ErrNotFound", err)
	}
}

// No tenant or reference can name a secret outside the tenant's own: each is
// refused before Secrets Manager is asked.
func TestNoReferenceLeavesItsTenant(t *testing.T) {
	for _, c := range []struct{ tenant, ref string }{
		{"t1", "../t2/kiff/payouts"},
		{"t1", "kiff/../../t2/kiff/payouts"},
		{"t1", "/t2/kiff/payouts"},
		{"t1", "kiff//payouts"},
		{"t1", "kiff/payouts/"},
		{"t1", "./kiff/payouts"},
		{"t1", ""},
		{"t1", "arn:aws:secretsmanager:region:account:secret:agents/t2/kiff/payouts"},
		{"t1", "kiff/pay outs"},
		{"t1/kiff", "payouts"}, // would read as tenant t1, reference kiff/payouts
		{"", "t1/kiff/payouts"},
		{"t1", strings.Repeat("a", 600)},
	} {
		f := newFake()
		s := secretsmanager.New(f, "agents/")
		if key, err := s.Key(ctx, c.tenant, c.ref); err == nil || len(f.asked) != 0 {
			t.Errorf("tenant %q, ref %q: got %q, %v, asked %v; want refused before asking", c.tenant, c.ref, key, err, f.asked)
		}
	}
}

// Secrets Manager may take a name ending in '-' and six characters as part of
// an ARN and answer with another secret. Only the secret asked for is a key.
func TestOnlyTheSecretAskedForIsAKey(t *testing.T) {
	f := newFake()
	f.secrets["agents/t1/kiff/payouts-abcdef"] = secret("agents/t1/kiff/payouts", "key-t1")
	s := secretsmanager.New(f, "agents/")
	if key, err := s.Key(ctx, "t1", "kiff/payouts-abcdef"); !errors.Is(err, secretsmanager.ErrNotFound) {
		t.Fatalf("got %q, %v; want ErrNotFound", key, err)
	}
}

func TestASecretWithoutAKeyIsRefused(t *testing.T) {
	f := newFake()
	f.secrets["agents/t1/blank"] = secret("agents/t1/blank", " \n")
	f.secrets["agents/t1/binary"] = &sm.GetSecretValueOutput{Name: aws.String("agents/t1/binary"), SecretBinary: []byte("key")}
	s := secretsmanager.New(f, "agents/")
	for _, ref := range []string{"blank", "binary"} {
		if key, err := s.Key(ctx, "t1", ref); err == nil {
			t.Errorf("%s: got %q, want an error", ref, key)
		}
	}
}

func TestAFailedReadIsReported(t *testing.T) {
	f := newFake()
	f.fail = errors.New("throttled")
	s := secretsmanager.New(f, "agents/")
	_, err := s.Key(ctx, "t1", "kiff/payouts")
	if err == nil || !errors.Is(err, f.fail) || errors.Is(err, secretsmanager.ErrNotFound) {
		t.Fatalf("err = %v, want the read's error, not ErrNotFound", err)
	}
}

func TestName(t *testing.T) {
	s := secretsmanager.New(&fake{}, "prod/agents/")
	got, err := s.Name("org_1", "kiff/refunds.v2")
	if err != nil || got != "prod/agents/org_1/kiff/refunds.v2" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// lister answers ListSecrets a page at a time, and keeps each filter asked.
type lister struct {
	pages   [][]types.SecretListEntry
	filters [][]types.Filter
	fail    error
}

func (l *lister) ListSecrets(_ context.Context, in *sm.ListSecretsInput, _ ...func(*sm.Options)) (*sm.ListSecretsOutput, error) {
	if l.fail != nil {
		return nil, l.fail
	}
	l.filters = append(l.filters, in.Filters)
	i := 0
	if in.NextToken != nil {
		i = int(aws.ToString(in.NextToken)[0] - '0')
	}
	out := &sm.ListSecretsOutput{SecretList: l.pages[i]}
	if i+1 < len(l.pages) {
		out.NextToken = aws.String(string(rune('0' + i + 1)))
	}
	return out, nil
}

func entry(name string, deleting bool) types.SecretListEntry {
	e := types.SecretListEntry{Name: aws.String(name)}
	if deleting {
		e.DeletedDate = aws.Time(time.Now())
	}
	return e
}

// A migration lists every key's secret under the prefix, across pages, by
// tenant and reference: one being deleted, another prefix's, or a name that
// isn't tenant/reference is left out.
func TestListReadsEveryKeysSecretAcrossPages(t *testing.T) {
	l := &lister{pages: [][]types.SecretListEntry{
		{entry("agents/t1/kiff", false), entry("agents/t1/gone", true), entry("agents-sealing/t1", false)},
		{entry("agents/t2/apps/stripe", false), entry("agents/t3", false), entry("agents/t4/..", false), entry("agents/t5/a b", false)},
	}}
	got, err := (&secretsmanager.Secrets{Prefix: "agents/"}).List(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	want := []secretsmanager.OldKey{{Tenant: "t1", Ref: "kiff"}, {Tenant: "t2", Ref: "apps/stripe"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("listed %v, want %v", got, want)
	}
	if len(l.filters) != 2 || len(l.filters[0]) != 1 || l.filters[0][0].Key != types.FilterNameStringTypeName || l.filters[0][0].Values[0] != "agents/" {
		t.Errorf("filters asked: %v", l.filters)
	}
	l.fail = errors.New("throttled")
	if _, err := (&secretsmanager.Secrets{Prefix: "agents/"}).List(ctx, l); err == nil {
		t.Error("a failed page listed nothing and no error")
	}
}

// The accounts' secrets are listed by account, across pages: one being
// deleted, or a name past the prefix that isn't one segment, is left out.
func TestAccountsListReadsEveryAccountsSecret(t *testing.T) {
	l := &lister{pages: [][]types.SecretListEntry{
		{entry("agents-accounts/t1", false), entry("agents-accounts/t2", true)},
		{entry("agents-accounts/t3", false), entry("agents-accounts/t4/x", false), entry("agents-accountsX", false)},
	}}
	got, err := secretsmanager.NewAccounts(nil, "agents-accounts/").List(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[t1 t3]" {
		t.Errorf("listed %v", got)
	}
}
