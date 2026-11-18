package secretsmanager_test

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/kiff/agents/registry/secretsmanager"
)

// retirable is Secrets Manager with recovery windows, as it behaves: a
// retired secret isn't read and isn't listed unless asked for, retiring it
// again is refused, and a force delete of it answers success and leaves it
// retired. Restoring brings it back.
type retirable struct {
	*fake
	retired    map[string]bool
	window     int64
	forceFails int   // force deletes that fail before one goes through
	restoreErr error // what RestoreSecret answers, when set
	restores   int
}

func newRetirable(names ...string) *retirable {
	r := &retirable{fake: &fake{secrets: map[string]*sm.GetSecretValueOutput{}}, retired: map[string]bool{}}
	for _, n := range names {
		r.secrets[n] = secret(n, "key")
	}
	return r
}

func markedForDeletion() error {
	return &types.InvalidRequestException{Message: aws.String("You can't perform this operation on the secret because it was marked for deletion.")}
}

func notFound() error {
	return &types.ResourceNotFoundException{Message: aws.String("Secrets Manager can't find the specified secret.")}
}

func (r *retirable) GetSecretValue(ctx context.Context, in *sm.GetSecretValueInput, o ...func(*sm.Options)) (*sm.GetSecretValueOutput, error) {
	if r.retired[aws.ToString(in.SecretId)] {
		return nil, markedForDeletion()
	}
	return r.fake.GetSecretValue(ctx, in, o...)
}

func (r *retirable) DeleteSecret(_ context.Context, in *sm.DeleteSecretInput, _ ...func(*sm.Options)) (*sm.DeleteSecretOutput, error) {
	id := aws.ToString(in.SecretId)
	if _, ok := r.secrets[id]; !ok {
		return nil, notFound()
	}
	if !aws.ToBool(in.ForceDeleteWithoutRecovery) {
		if r.retired[id] {
			return nil, markedForDeletion()
		}
		r.window = aws.ToInt64(in.RecoveryWindowInDays)
		r.retired[id] = true
		return &sm.DeleteSecretOutput{}, nil
	}
	if r.retired[id] {
		// It looks deleted, and it isn't.
		return &sm.DeleteSecretOutput{}, nil
	}
	if r.forceFails > 0 {
		r.forceFails--
		return nil, errors.New("throttled")
	}
	delete(r.secrets, id)
	return &sm.DeleteSecretOutput{}, nil
}

func (r *retirable) RestoreSecret(_ context.Context, in *sm.RestoreSecretInput, _ ...func(*sm.Options)) (*sm.RestoreSecretOutput, error) {
	r.restores++
	if r.restoreErr != nil {
		return nil, r.restoreErr
	}
	id := aws.ToString(in.SecretId)
	if _, ok := r.secrets[id]; !ok {
		return nil, notFound()
	}
	delete(r.retired, id)
	return &sm.RestoreSecretOutput{}, nil
}

func (r *retirable) ListSecrets(_ context.Context, in *sm.ListSecretsInput, _ ...func(*sm.Options)) (*sm.ListSecretsOutput, error) {
	want := in.Filters[0].Values[0]
	var names []string
	for n := range r.secrets {
		if r.retired[n] && !aws.ToBool(in.IncludePlannedDeletion) {
			continue
		}
		if strings.Contains(n, strings.TrimSuffix(want, "/")) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	out := &sm.ListSecretsOutput{}
	for _, n := range names {
		e := types.SecretListEntry{Name: aws.String(n)}
		if r.retired[n] {
			e.DeletedDate = aws.Time(time.Now())
		}
		out.SecretList = append(out.SecretList, e)
	}
	return out, nil
}

// left is every secret still there, retired ones too.
func (r *retirable) left() []string {
	var names []string
	for n := range r.secrets {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// A moved or replaced key's secret is retired: not read any more, kept
// RecoveryDays to restore. Retired again, or not there, it's done.
func TestRetireKeepsTheSecretToRestore(t *testing.T) {
	r := newRetirable("agents/t1/kiff/taf_1")
	old := secretsmanager.NewOldKeys(secretsmanager.New(r, "agents/"), r)
	if err := old.Retire(ctx, "t1", "kiff/taf_1"); err != nil {
		t.Fatal(err)
	}
	if !r.retired["agents/t1/kiff/taf_1"] || r.window != secretsmanager.RecoveryDays {
		t.Fatalf("retired %v, window %d", r.retired, r.window)
	}
	if _, err := old.Key(ctx, "t1", "kiff/taf_1"); !errors.Is(err, secretsmanager.ErrNotFound) {
		t.Fatalf("a retired secret read: %v", err)
	}
	if err := old.Retire(ctx, "t1", "kiff/taf_1"); err != nil {
		t.Fatalf("retired again: %v", err)
	}
	if err := old.Retire(ctx, "t1", "kiff/none"); err != nil {
		t.Fatalf("none there: %v", err)
	}
}

// A removed key's secret is deleted at once, a retired one too: a force
// delete alone would leave it retired, so it's restored first.
func TestDeleteTakesARetiredSecretAtOnce(t *testing.T) {
	r := newRetirable("agents/t1/kiff/taf_1", "agents/t1/kiff/taf_2")
	old := secretsmanager.NewOldKeys(secretsmanager.New(r, "agents/"), r)
	if err := old.Retire(ctx, "t1", "kiff/taf_1"); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"kiff/taf_1", "kiff/taf_2", "kiff/none"} {
		if err := old.Delete(ctx, "t1", ref); err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
	}
	if left := r.left(); len(left) != 0 {
		t.Fatalf("left %v", left)
	}
}

// A restored secret is never left live: its delete is tried again, and if
// no try goes through, it's retired again and the error answered.
func TestARestoredSecretIsNeverLeftLive(t *testing.T) {
	r := newRetirable("agents/t1/kiff/taf_1")
	old := secretsmanager.NewOldKeys(secretsmanager.New(r, "agents/"), r)
	if err := old.Retire(ctx, "t1", "kiff/taf_1"); err != nil {
		t.Fatal(err)
	}
	r.forceFails = 2
	if err := old.Delete(ctx, "t1", "kiff/taf_1"); err != nil || len(r.left()) != 0 {
		t.Fatalf("two failed tries, then: %v, left %v", err, r.left())
	}

	r = newRetirable("agents/t1/kiff/taf_1")
	old = secretsmanager.NewOldKeys(secretsmanager.New(r, "agents/"), r)
	if err := old.Retire(ctx, "t1", "kiff/taf_1"); err != nil {
		t.Fatal(err)
	}
	r.forceFails = 99
	if err := old.Delete(ctx, "t1", "kiff/taf_1"); err == nil {
		t.Fatal("a secret no try deleted answered done")
	}
	if !r.retired["agents/t1/kiff/taf_1"] {
		t.Fatal("the restored secret was left live")
	}
	if _, err := old.Key(ctx, "t1", "kiff/taf_1"); !errors.Is(err, secretsmanager.ErrNotFound) {
		t.Fatalf("read after the failure: %v", err)
	}
}

// A restore refused (no permission, throttled) deletes nothing, answers the
// error, and leaves a retired secret retired.
func TestARefusedRestoreLeavesItRetired(t *testing.T) {
	r := newRetirable("agents/t1/kiff/taf_1")
	old := secretsmanager.NewOldKeys(secretsmanager.New(r, "agents/"), r)
	if err := old.Retire(ctx, "t1", "kiff/taf_1"); err != nil {
		t.Fatal(err)
	}
	r.restoreErr = errors.New("AccessDeniedException")
	if err := old.Delete(ctx, "t1", "kiff/taf_1"); err == nil {
		t.Fatal("a refused restore answered done")
	}
	if !r.retired["agents/t1/kiff/taf_1"] {
		t.Fatal("left live")
	}
}

// An erased account keeps nothing: its keys' retired secrets are deleted at
// once too, and listed with those scheduled for deletion, none of its own
// is left. Another tenant's, retired or not, stays.
func TestEraseTakesRetiredKeySecrets(t *testing.T) {
	r := newRetirable("agents/t1/kiff/taf_1", "agents/t1/kiff/taf_2", "agents/t10/kiff/taf_1", "agents/t2/kiff/taf_1")
	for _, n := range []string{"agents/t1/kiff/taf_1", "agents/t2/kiff/taf_1"} {
		if _, err := r.DeleteSecret(ctx, &sm.DeleteSecretInput{SecretId: aws.String(n), RecoveryWindowInDays: aws.Int64(7)}); err != nil {
			t.Fatal(err)
		}
	}
	er := secretsmanager.NewEraser(r, "", "")
	er.KeysPrefix = "agents/"
	if err := er.Erase(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	out, err := r.ListSecrets(ctx, &sm.ListSecretsInput{
		Filters:                []types.Filter{{Key: types.FilterNameStringTypeName, Values: []string{"agents/t1/"}}},
		IncludePlannedDeletion: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range out.SecretList {
		if strings.HasPrefix(aws.ToString(s.Name), "agents/t1/") {
			t.Errorf("left after the erase: %s (retired %v)", aws.ToString(s.Name), s.DeletedDate != nil)
		}
	}
	if want := []string{"agents/t10/kiff/taf_1", "agents/t2/kiff/taf_1"}; !slices.Equal(r.left(), want) || !r.retired["agents/t2/kiff/taf_1"] {
		t.Fatalf("left %v (retired %v), want %v", r.left(), r.retired, want)
	}
	// A retired secret that can't be deleted fails the erase, for it to be
	// tried again, and is retired again.
	r = newRetirable("agents/t1/kiff/taf_1")
	if _, err := r.DeleteSecret(ctx, &sm.DeleteSecretInput{SecretId: aws.String("agents/t1/kiff/taf_1"), RecoveryWindowInDays: aws.Int64(7)}); err != nil {
		t.Fatal(err)
	}
	r.forceFails = 99
	er = secretsmanager.NewEraser(r, "", "")
	er.KeysPrefix = "agents/"
	if err := er.Erase(ctx, "t1"); err == nil || !r.retired["agents/t1/kiff/taf_1"] {
		t.Fatalf("erase %v, retired %v", err, r.retired)
	}
}
