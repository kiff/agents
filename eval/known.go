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

// MaxKnownDays is how long a miss can stay known.
const MaxKnownDays = 3

// Known is one known miss. It holds for the check in every suite, so keep
// one only as long as its fix takes.
type Known struct {
	Check string `json:"check"`
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
	if until.Before(since) || until.Sub(since) > MaxKnownDays*24*time.Hour {
		return fmt.Errorf("until is more than %d days after since, or before it", MaxKnownDays)
	}
	if k.Fix == "" || k.Repro == "" {
		return fmt.Errorf("names no fix or no repro test")
	}
	return nil
}

// KnownMiss is the known entry that covers check on day now, if any: from
// since to until, both days included, in UTC.
func KnownMiss(known []Known, check string, now time.Time) (Known, bool) {
	day := now.UTC().Format(time.DateOnly)
	for _, k := range known {
		if k.Check == check && k.Since <= day && day <= k.Until {
			return k, true
		}
	}
	return Known{}, false
}
