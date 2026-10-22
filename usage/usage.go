// Package usage keeps what an account's chats with its agents cost the model,
// per billing period, for AI fair use (kiff/tafs#193): a chat or Setup run
// past the period's bound doesn't start.
//
// The caller names the period and the bound with each run it starts (tafs
// cloud, from the account's plan). The owner's go on is KIFF's, signed on
// their phone: tafs cloud then sends no bound. This package knows neither
// plans nor currencies, only micro US dollars.
package usage

import (
	"context"
	"time"
)

// Period is what an account's counted runs cost in one billing period.
type Period struct {
	// Spent is the counted runs' model cost so far, in millionths of a US
	// dollar.
	Spent int64 `json:"spent_micro_usd"`
}

// Store keeps each account's periods. Every call is scoped by tenant. A
// period is the caller's opaque name for it, such as its start.
type Store interface {
	// Add adds microUSD to the period's spend, atomically, and keeps the
	// period until until.
	Add(ctx context.Context, tenant, period string, microUSD int64, until time.Time) error
	// Get returns the period: zero if nothing was spent.
	Get(ctx context.Context, tenant, period string) (Period, error)
	// EraseTenant deletes every period of the tenant, when its account is
	// deleted.
	EraseTenant(ctx context.Context, tenant string) error
}

// Allowed reports whether p leaves room for one more counted run under bound:
// spend below it. A bound of zero or less is none.
func Allowed(p Period, bound int64) bool {
	return bound <= 0 || p.Spent < bound
}
