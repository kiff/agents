package secretsmanager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// RecoveryDays is how long a retired secret can be restored: Secrets
// Manager's shortest window.
const RecoveryDays = 7

// RestoreAPI is the part of the Secrets Manager client that deletes a
// retired secret at once.
type RestoreAPI interface {
	DeleteAPI
	RestoreSecret(context.Context, *sm.RestoreSecretInput, ...func(*sm.Options)) (*sm.RestoreSecretOutput, error)
}

// purgeTries is how many times a restored secret's delete is tried, and
// purgeWait the wait before each try after the first, times its number.
var (
	purgeTries = 3
	purgeWait  = 100 * time.Millisecond
)

// retire deletes the secret with the recovery window. None there, or one
// already being deleted, is not an error.
func retire(ctx context.Context, api DeleteAPI, name string) error {
	_, err := api.DeleteSecret(ctx, &sm.DeleteSecretInput{SecretId: aws.String(name), RecoveryWindowInDays: aws.Int64(RecoveryDays)})
	if err != nil && !missing(err) && !deleting(err) {
		return err
	}
	return nil
}

// purge deletes the secret at once, a retired one too. A force delete of a
// secret already scheduled for deletion answers success and leaves it
// scheduled, so the secret is restored first, then force deleted. If no try
// of that delete succeeds, the secret is retired again, never left live, and
// the error is answered. None there is not an error.
//
// A restored key's secret is read by nothing for that moment: a retired
// secret's key is in the table, which is read first.
func purge(ctx context.Context, api RestoreAPI, name string) error {
	_, err := api.RestoreSecret(ctx, &sm.RestoreSecretInput{SecretId: aws.String(name)})
	var invalid *types.InvalidRequestException
	switch {
	case missing(err):
		return nil
	case err != nil && !errors.As(err, &invalid):
		// Not restored: if it was retired, it still is.
		return fmt.Errorf("restoring %s to delete it: %w", name, err)
	}
	// Restored, or it wasn't retired: either way, deleted at once now.
	for try := 0; try < purgeTries; try++ {
		if try > 0 {
			time.Sleep(time.Duration(try) * purgeWait)
		}
		_, err = api.DeleteSecret(ctx, &sm.DeleteSecretInput{SecretId: aws.String(name), ForceDeleteWithoutRecovery: aws.Bool(true)})
		if err == nil || missing(err) {
			return nil
		}
	}
	if rerr := retire(ctx, api, name); rerr != nil {
		return fmt.Errorf("deleting %s: %w; and it was left restored: %v", name, err, rerr)
	}
	return fmt.Errorf("deleting %s, retired again: %w", name, err)
}

func missing(err error) bool {
	var nf *types.ResourceNotFoundException
	return errors.As(err, &nf)
}
