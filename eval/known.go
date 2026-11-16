package eval

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"time"
)

// A known miss is an experience gate below the bar while its fix lands
// (decided 16 Nov 2026): the Eval reports it as known instead of failing on
// it, until its date. Only an experience gate can be one, never a blocking
// gate; for at most three days; and only with its fix and a deterministic
// test that reproduces the miss, so it can't become a standing waiver.

//go:embed known.json
var knownJSON []byte

// MaxKnownDays is how many days a miss can stay known, both its first and
// last day counted: since 16 Nov, until 18 Nov at the latest.
const MaxKnownDays = 3

// Known is one known miss: one check in one case, so the same check missing
// in another case still fails.
type Known struct {
	Check string `json:"check"`
	// Case is where it missed: cmd/eval's case (chargebacks) or cmd/e2e's
	// (setup, desk, invoices, replay-intros, plan-sheets, plan-notion),
	// as the tallies name it.
	Case  string `json:"case"`
	Since string `json:"since"` // YYYY-MM-DD
	Until string `json:"until"` // YYYY-MM-DD, its last day
	Fix   string `json:"fix"`   // the fix's PR, as owner/repo#N
	Repro string `json:"repro"` // the test that reproduces the miss
}

// LoadKnown reads known.json and checks each entry.
func LoadKnown() ([]Known, error) {
	var f struct {
		Known []Known `json:"known"`
	}
	if err := json.Unmarshal(knownJSON, &f); err != nil {
		return nil, fmt.Errorf("known.json: %w", err)
	}
	for _, k := range f.Known {
		if err := k.check(); err != nil {
			return nil, fmt.Errorf("known.json: %q: %w", k.Check, err)
		}
	}
	return f.Known, nil
}

func (k Known) check() error {
	if !QualityGate(k.Check) {
		return fmt.Errorf("not an experience gate: a blocking gate is never known")
	}
	since, err := time.Parse(time.DateOnly, k.Since)
	if err != nil {
		return fmt.Errorf("since: %w", err)
	}
	until, err := time.Parse(time.DateOnly, k.Until)
	if err != nil {
		return fmt.Errorf("until: %w", err)
	}
	if until.Before(since) || until.Sub(since) > (MaxKnownDays-1)*24*time.Hour {
		return fmt.Errorf("since to until is more than %d days, both counted, or backwards", MaxKnownDays)
	}
	if k.Case == "" || k.Fix == "" || k.Repro == "" {
		return fmt.Errorf("names no case, no fix or no repro test")
	}
	return nil
}

// KnownMiss is the known entry that covers check in case on day now, if
// any: from since to until, both days included, in UTC.
func KnownMiss(known []Known, check, caseName string, now time.Time) (Known, bool) {
	day := now.UTC().Format(time.DateOnly)
	for _, k := range known {
		if k.Check == check && k.Case == caseName && k.Since <= day && day <= k.Until {
			return k, true
		}
	}
	return Known{}, false
}
