package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/model/anthropic"
	"github.com/kiff/agents/model/bedrock"
	"github.com/kiff/agents/usage"
)

// sourceConfig is where runs' turns go (model.Sourced), from the function's
// configuration. Everything is off by default, so turns go to the Claude API
// with KIFF's key, as before:
//
//	AGENTS_MODEL_SOURCE       anthropic (the default) or bedrock: where new runs go
//	AGENTS_BEDROCK_FALLBACK   on: a new run whose first turn the Claude API refuses
//	                          for credits, or can't serve, goes to Bedrock
//	AGENTS_BEDROCK_DAILY_USD  the most Bedrock's turns may cost in a UTC day, in
//	                          US dollars; without it, Bedrock refuses every turn
//	AGENTS_BEDROCK_ACCOUNT_DAILY_USD
//	                          each account's share of that day, in US dollars;
//	                          without it, AccountShare of the cap
//	AGENTS_BEDROCK_RESERVED_TENANTS
//	                          the accounts (tenant ids, comma-separated) the
//	                          reserved slice is for: the store reviewers'.
//	                          Set on purpose, empty by default
//	AGENTS_BEDROCK_RESERVED_USD
//	                          the slice of the cap only those accounts may
//	                          spend; without it, ReservedShare of the cap when
//	                          any are listed
//
// Bedrock is our own AWS account's, in the function's region, through the EU
// inference profiles (model/bedrock), signed with the function's own role.
type sourceConfig struct {
	Source       string
	Fallback     bool
	CapMicro     int64
	AccountMicro int64
	ReservedTo   map[string]bool
	ReserveMicro int64
}

// The defaults, in percent of the day's cap (docs/runs.md says why):
//   - AccountShare: one account's share, so at least ten accounts must
//     spend all of theirs to stop the rest.
//   - ReservedShare: the reviewers' slice, when any are listed: two full
//     shares.
const (
	AccountShare  = 10
	ReservedShare = 20
)

func sourceConfigFrom(getenv func(string) string) (sourceConfig, error) {
	var sc sourceConfig
	switch v := strings.TrimSpace(getenv("AGENTS_MODEL_SOURCE")); v {
	case "", model.SourceAnthropic:
		sc.Source = model.SourceAnthropic
	case model.SourceBedrock:
		sc.Source = model.SourceBedrock
	default:
		return sc, fmt.Errorf("lambda: AGENTS_MODEL_SOURCE is anthropic or bedrock, not %q", v)
	}
	switch v := strings.TrimSpace(getenv("AGENTS_BEDROCK_FALLBACK")); v {
	case "", "off":
	case "on":
		sc.Fallback = true
	default:
		return sc, fmt.Errorf("lambda: AGENTS_BEDROCK_FALLBACK is on or off, not %q", v)
	}
	if v := strings.TrimSpace(getenv("AGENTS_BEDROCK_DAILY_USD")); v != "" {
		usd, err := strconv.ParseFloat(v, 64)
		if err != nil || usd <= 0 || usd > 100_000 {
			return sc, fmt.Errorf("lambda: AGENTS_BEDROCK_DAILY_USD is a number of US dollars above 0, not %q", v)
		}
		sc.CapMicro = int64(usd * 1_000_000)
	}
	var err error
	if sc.AccountMicro, err = dollars(getenv, "AGENTS_BEDROCK_ACCOUNT_DAILY_USD", sc.CapMicro*AccountShare/100); err != nil {
		return sc, err
	}
	for _, t := range strings.Split(getenv("AGENTS_BEDROCK_RESERVED_TENANTS"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			if sc.ReservedTo == nil {
				sc.ReservedTo = map[string]bool{}
			}
			sc.ReservedTo[t] = true
		}
	}
	if len(sc.ReservedTo) > 0 {
		if sc.ReserveMicro, err = dollars(getenv, "AGENTS_BEDROCK_RESERVED_USD", sc.CapMicro*ReservedShare/100); err != nil {
			return sc, err
		}
	}
	if sc.CapMicro > 0 && (sc.AccountMicro > sc.CapMicro || sc.ReserveMicro >= sc.CapMicro) {
		return sc, fmt.Errorf("lambda: an account's share and the reserved slice are parts of AGENTS_BEDROCK_DAILY_USD, below it")
	}
	return sc, nil
}

// dollars is name's amount of US dollars, in millionths, or def when unset.
func dollars(getenv func(string) string, name string, def int64) (int64, error) {
	v := strings.TrimSpace(getenv(name))
	if v == "" {
		return def, nil
	}
	usd, err := strconv.ParseFloat(v, 64)
	if err != nil || usd <= 0 || usd > 100_000 {
		return 0, fmt.Errorf("lambda: %s is a number of US dollars above 0, not %q", name, v)
	}
	return int64(usd * 1_000_000), nil
}

// sourced is the model runs use: primary alone when Bedrock isn't configured,
// exactly as before; otherwise model.Sourced over primary and Bedrock, which
// says to log a turn that cost more than its worst case.
func sourced(primary model.Model, sc sourceConfig, cfg aws.Config, use usage.Store, log func(msg string, err error)) model.Model {
	if sc.Source != model.SourceBedrock && !sc.Fallback {
		return primary
	}
	s := &model.Sourced{Primary: primary, Backup: bedrock.New(cfg), Source: sc.Source, Fallback: sc.Fallback, FallsBack: anthropic.FallsBack, Log: log}
	if sc.CapMicro > 0 && use != nil {
		s.Cap = &model.DailyCap{MicroUSD: sc.CapMicro, AccountMicroUSD: sc.AccountMicro, ReservedMicroUSD: sc.ReserveMicro, Reserved: sc.ReservedTo,
			Spend: daySpend{use: use, log: log}, PriceFactor: bedrock.GeoFactor}
	}
	return s
}

// backupTenant is the usage key Bedrock's day totals are kept under: no
// tenant ID has a "#".
const backupTenant = "#bedrock"

// shareKey is where an account's part of Bedrock's day is kept: in its own
// usage (so erasing the account erases it), as the period "bedrock|<day>",
// which no billing period is. A turn for no account counts under
// "#untenanted", one share for all such turns.
func shareKey(tenant, day string) (string, string) {
	if tenant == "" {
		tenant = "#untenanted"
	}
	return tenant, "bedrock|" + day
}

// The lines the function logs as Bedrock's day nears and reaches its cap, for
// an alarm to tell the owner (docs/runs.md): past the cap, every turn is
// refused until 00:00 UTC.
const (
	// CapNear: the day's spend, reservations included, passed CapNearShare
	// of the cap. Said once, by the turn that crossed it.
	CapNear = "bedrock: today's spend passed 80% of its cap"
	// CapReached: a turn was refused for the cap. Said for each one.
	CapReached = "bedrock: a turn was refused, today's cap is spent"
	// ShareReached: a turn was refused for its account's share. Said for
	// each one, with the account.
	ShareReached = "bedrock: a turn was refused, its account's share of today's cap is spent"
	// CapNearShare is CapNear's share of the cap, in percent.
	CapNearShare = 80
)

// daySpend keeps Bedrock's day totals in the usage table, each for three days,
// and says (log, when set) when the day nears and reaches its cap.
type daySpend struct {
	use usage.Store
	log func(msg string, err error)
}

// Reserve adds a turn's worst case to its account's share, while that stays
// within share, then to the day, while that stays within limit. When the day
// has no room, the share is given back: both, or neither. Each is one
// conditional update, so the one item every turn writes, the day's, takes
// a plain update, never a transaction that concurrent turns would cancel.
// A give-back that fails leaves the account's share counted, the safe side.
func (d daySpend) Reserve(ctx context.Context, day, tenant string, microUSD, limit, share int64) (model.Full, error) {
	until, err := dayKept(day)
	if err != nil {
		return model.NotFull, err
	}
	who, period := shareKey(tenant, day)
	if share <= 0 { // no share: the account's part is only counted
		if err := d.use.Add(ctx, who, period, microUSD, until); err != nil {
			return model.NotFull, err
		}
	} else {
		ok, err := d.use.Reserve(ctx, who, period, microUSD, share, until)
		if err != nil {
			return model.NotFull, err
		}
		if !ok {
			if d.log != nil {
				d.log(ShareReached, fmt.Errorf("%s, account %s: a turn's worst case of %d micro-USD would pass its share of %d", day, who, microUSD, share))
			}
			return model.ShareFull, nil
		}
	}
	ok, err := d.use.Reserve(ctx, backupTenant, day, microUSD, limit, until)
	if err != nil || !ok {
		if berr := d.use.Add(ctx, who, period, -microUSD, until); berr != nil {
			err = errors.Join(err, fmt.Errorf("lambda: giving back account %s's share: %w", who, berr))
		}
	}
	if err != nil {
		return model.NotFull, err
	}
	if !ok {
		if d.log != nil {
			d.log(CapReached, fmt.Errorf("%s: a turn's worst case of %d micro-USD would pass the cap of %d", day, microUSD, limit))
		}
		return model.DayFull, nil
	}
	if d.log == nil {
		return model.NotFull, nil
	}
	// The turn that took the day across the line says so: what was spent
	// before it was below, what's spent with it isn't. A read that fails
	// says nothing; the cap itself still holds.
	if p, gerr := d.use.Get(ctx, backupTenant, day); gerr == nil {
		near := limit * CapNearShare / 100
		if p.Spent >= near && p.Spent-microUSD < near {
			d.log(CapNear, fmt.Errorf("%s: %d of %d micro-USD, reservations included", day, p.Spent, limit))
		}
	}
	return model.NotFull, nil
}

// Add settles a turn, on the day and on its account's share: what it cost
// less what it reserved, often negative.
func (d daySpend) Add(ctx context.Context, day, tenant string, microUSD int64) error {
	until, err := dayKept(day)
	if err != nil {
		return err
	}
	who, period := shareKey(tenant, day)
	return errors.Join(d.use.Add(ctx, backupTenant, day, microUSD, until), d.use.Add(ctx, who, period, microUSD, until))
}

// dayKept is how long a day's total is kept: three days past it.
func dayKept(day string) (time.Time, error) {
	t, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return time.Time{}, err
	}
	return t.Add(72 * time.Hour), nil
}
