package dynamodb

import (
	"cmp"
	"context"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/kiff/agents/session"
)

// Parallel is how many requests one call runs at a time, when it has several
// (deleting an agent's sessions, reading many sessions for List).
const Parallel = 4

// chunks runs fn over items in chunks of size, Parallel at a time. The first
// error cancels the rest and is returned.
func chunks[T any](ctx context.Context, items []T, size int, fn func(context.Context, []T) error) error {
	if len(items) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	slots := make(chan struct{}, Parallel)
	var wg sync.WaitGroup
	for start := 0; start < len(items); start += size {
		chunk := items[start:min(start+size, len(items))]
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			if err := fn(ctx, chunk); err != nil {
				cancel(err)
			}
		}()
	}
	wg.Wait()
	if err := context.Cause(ctx); err != nil && err != context.Canceled {
		return err
	}
	return ctx.Err()
}

// backoff waits before retry number try (1, 2, …): exponential with jitter,
// as DynamoDB asks for unprocessed items, at most about two seconds.
func backoff(ctx context.Context, try int) error {
	if try > 8 {
		return errThrottled
	}
	d := time.Duration(rand.Int64N(int64(25*time.Millisecond) << min(try, 6)))
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type throttled struct{}

func (throttled) Error() string { return "dynamodb: items still unprocessed after retries" }

var errThrottled error = throttled{}

func sortNewestFirst(out []session.Session) {
	slices.SortFunc(out, func(a, b session.Session) int { return cmp.Or(b.Updated.Compare(a.Updated), cmp.Compare(a.ID, b.ID)) })
}
