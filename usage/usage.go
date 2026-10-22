// Package usage keeps what an account's chats with its agents cost the model,
// per billing period, for AI fair use (kiff/tafs#193): a chat or Setup run
// past the period's bound doesn't start until the owner says go on.
//
// The caller names the period and the bound with each run it starts (tafs
// cloud, from the account's plan); this package knows neither plans nor
// currencies, only micro US dollars.
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
	// GoOn records that the owner said go on past the bound, for this period.
	GoOn bool `json:"go_on"`
}

// Store keeps each account's periods. Every call is scoped by tenant. A
// period is the caller's opaque name for it, such as its start.
type Store interface {
	// Add adds microUSD to the period's spend, atomically, and keeps the
	// period until until.
	Add(ctx context.Context, tenant, period string, microUSD int64, until time.Time) error
	// Get returns the period: zero if nothing was spent and nobody said go on.
	Get(ctx context.Context, tenant, period string) (Period, error)
	// GoOn records the owner's go on for the period, keeping it until until,
	// and returns the period as stored. Again is not an error.
	GoOn(ctx context.Context, tenant, period string, until time.Time) (Period, error)
	// EraseTenant deletes every period of the tenant, when its account is
	// deleted.
	EraseTenant(ctx context.Context, tenant string) error
}

// Allowed reports whether p leaves room for one more counted run under bound:
// spend below it, or the owner's go on. A bound of zero or less is none.
func Allowed(p Period, bound int64) bool {
	return bound <= 0 || p.GoOn || p.Spent < bound
}
