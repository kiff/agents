package usage

import (
	"context"
	"time"

	"github.com/kiff/agents/run"
)

// What runs spent, summed per UTC day, so the owner can watch the cost while
// testing without one read per run: per provider, agent and where the run
// came from (its source). A run is added as it pauses or ends: its turns,
// tokens and cost since it was last stored, and at its end the run itself
// and its credits.

// DaysKeep is how long a day's rows are kept.
const DaysKeep = 90 * 24 * time.Hour

// DayKey is one row: a UTC day ("2006-01-02"), the provider the turns ran
// on, the agent, and the run's source.
type DayKey struct {
	Day, Provider, Agent, Source string
}

// Day is what a row counts. Every field is added to, never set.
type Day struct {
	Runs         int64 `json:"runs"`
	Turns        int64 `json:"turns"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	CacheRead    int64 `json:"cache_read_tokens"`
	CacheWrite   int64 `json:"cache_write_tokens"`
	CacheWrite1h int64 `json:"cache_write_1h_tokens"`
	// CostMicroUSD is the turns' cost at the price where they ran (Bedrock's
	// EU profiles included), in millionths of a US dollar.
	CostMicroUSD int64 `json:"cost_micro_usd"`
	// CreditsList are the ended runs' credits, each worked out as KIFF does
	// (Credits) on its cost, free or not.
	CreditsList int64 `json:"credits_list"`
	// CreditsCharged are those of the ended runs KIFF doesn't make free
	// (Charged): the gap to CreditsList is what failures and trials cost
	// without being charged. Setup's runs are in it as charged: KIFF makes
	// them free within its period's allowance, which only KIFF knows, so on
	// Setup's rows it's at most what was charged.
	CreditsCharged int64 `json:"credits_charged"`
}

// DayRow is one row as read.
type DayRow struct {
	DayKey
	Day
}

// Days keeps the rows.
type Days interface {
	// AddDay adds d to k's row, atomically, and keeps it until until.
	AddDay(ctx context.Context, k DayKey, d Day, until time.Time) error
	// DayRows returns every row of one UTC day.
	DayRows(ctx context.Context, day string) ([]DayRow, error)
}

// Credits is what KIFF charges a run whose AI cost costMicroUSD, as its
// meter.Credits works it out (kiff/cloud api/internal/meter/runs.go): the
// cost read in euros at a fixed 1.08 US dollars, times 4 credits a euro
// cent, rounded up once per run. KIFF's is the one that's charged; this one
// only measures, and its test pins KIFF's examples.
func Credits(costMicroUSD int64) int64 {
	if costMicroUSD <= 0 {
		return 0
	}
	num := costMicroUSD * 100 * 100 * 4
	den := int64(1_000_000 * 108)
	return (num + den - 1) / den
}

// Charged is whether KIFF charges a run that ended with status and trigger,
// as meter.ClassifyRun does (kiff/cloud api/internal/meter/runs.go): a trial
// is free, and so is a failed run, which is every ERROR (kiff.UsageOf).
// Setup's allowance is KIFF's alone and isn't read here (CreditsCharged).
func Charged(status run.Status, trigger run.Trigger) bool {
	return trigger != run.ByTrial && status != run.Error
}
