package secretsmanager_test

import (
	"errors"
	"strings"
	"testing"

	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/kiff/agents/registry/secretsmanager"
	"github.com/kiff/agents/server/kiffalert"
)

func newAccounts() (*fake, *secretsmanager.Accounts) {
	f := &fake{secrets: map[string]*sm.GetSecretValueOutput{
		"kiff-accounts/acct_1": secret("kiff-accounts/acct_1", `{"tenant": "t1", "secret": "whsec-1"}`),
		"kiff-accounts/acct_2": secret("kiff-accounts/acct_2", `{"tenant": "t2", "secret": "whsec-2"}`),
	}}
	return f, secretsmanager.NewAccounts(f, "kiff-accounts/")
}

func TestAnAccountIsItsTenantAndSecret(t *testing.T) {
	_, a := newAccounts()
	for account, want := range map[string][2]string{"acct_1": {"t1", "whsec-1"}, "acct_2": {"t2", "whsec-2"}} {
		tenant, secret, err := a.Account(ctx, account)
		if err != nil || tenant != want[0] || secret != want[1] {
			t.Errorf("%s: %q %q (%v), want %q", account, tenant, secret, err, want)
		}
	}
}

// An alert names its account, unsigned until the account's secret checks it:
// it can't reach a secret outside the prefix, and an unknown one is the same
// answer as a name that isn't one.
func TestAnAccountIsOnlyOneUnderThePrefix(t *testing.T) {
	f, a := newAccounts()
	f.secrets["agents/t1/kiff/payouts"] = secret("agents/t1/kiff/payouts", `{"tenant": "t1", "secret": "x"}`)
	for _, account := range []string{"acct_3", "", ".", "..", "../agents/t1/kiff/payouts", "acct_1/x", strings.Repeat("a", 600)} {
		if _, _, err := a.Account(ctx, account); !errors.Is(err, kiffalert.ErrUnknownAccount) {
			t.Errorf("%q: %v, want ErrUnknownAccount", account, err)
		}
	}
	// Only acct_3 is a name worth asking for. One too long for Secrets Manager
	// would be a ValidationException there: a 503, not the 401 promised.
	if len(f.asked) != 1 || f.asked[0] != "kiff-accounts/acct_3" {
		t.Errorf("asked for %q, want only kiff-accounts/acct_3", f.asked)
	}
}

// Secrets Manager may answer a name ending in '-' and six characters with
// another secret, as part of an ARN.
func TestOnlyTheAccountAskedForAnswers(t *testing.T) {
	f, a := newAccounts()
	f.secrets["kiff-accounts/acct-abcdef"] = secret("kiff-accounts/acct", `{"tenant": "t1", "secret": "whsec-1"}`)
	if _, _, err := a.Account(ctx, "acct-abcdef"); !errors.Is(err, kiffalert.ErrUnknownAccount) {
		t.Fatalf("%v, want ErrUnknownAccount", err)
	}
}

func TestAnAccountSecretMustNameATenantAndASecret(t *testing.T) {
	f, a := newAccounts()
	for _, v := range []string{`whsec-1`, `{"tenant": "t1"}`, `{"secret": "whsec-1"}`, `{"tenant": "t1/x", "secret": "whsec-1"}`} {
		f.secrets["kiff-accounts/bad"] = secret("kiff-accounts/bad", v)
		_, _, err := a.Account(ctx, "bad")
		if err == nil || errors.Is(err, kiffalert.ErrUnknownAccount) || strings.Contains(err.Error(), "whsec") {
			t.Errorf("%s: %v, want a configuration error that carries no secret", v, err)
		}
	}
}

func TestAFailedAccountReadIsReported(t *testing.T) {
	f, a := newAccounts()
	f.fail = errors.New("throttled")
	if _, _, err := a.Account(ctx, "acct_1"); err == nil || errors.Is(err, kiffalert.ErrUnknownAccount) {
		t.Fatalf("%v, want the read's error", err)
	}
}
