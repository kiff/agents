package main

import (
	"context"
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
//
// Bedrock is our own AWS account's, in the function's region, through the EU
// inference profiles (model/bedrock), signed with the function's own role.
type sourceConfig struct {
	Source   string
	Fallback bool
	CapMicro int64
}

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
	return sc, nil
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
		s.Cap = &model.DailyCap{MicroUSD: sc.CapMicro, Spend: daySpend{use}, PriceFactor: bedrock.GeoFactor}
	}
	return s
}

// backupTenant is the usage key Bedrock's day totals are kept under: no
// tenant ID has a "#".
const backupTenant = "#bedrock"

// daySpend keeps Bedrock's day totals in the usage table, each for three days.
type daySpend struct{ use usage.Store }

// Reserve adds a turn's worst case to the day only while it stays within limit.
func (d daySpend) Reserve(ctx context.Context, day string, microUSD, limit int64) (bool, error) {
	until, err := dayKept(day)
	if err != nil {
		return false, err
	}
	return d.use.Reserve(ctx, backupTenant, day, microUSD, limit, until)
}

// Add settles a turn: what it cost less what it reserved, often negative.
func (d daySpend) Add(ctx context.Context, day string, microUSD int64) error {
	until, err := dayKept(day)
	if err != nil {
		return err
	}
	return d.use.Add(ctx, backupTenant, day, microUSD, until)
}

// dayKept is how long a day's total is kept: three days past it.
func dayKept(day string) (time.Time, error) {
	t, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return time.Time{}, err
	}
	return t.Add(72 * time.Hour), nil
}
