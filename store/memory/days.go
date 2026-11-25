package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/kiff/agents/usage"
)

// Days is a usage.Days held in memory.
type Days struct {
	mu   sync.Mutex
	rows map[usage.DayKey]usage.Day
}

// NewDays returns empty rows.
func NewDays() *Days { return &Days{rows: map[usage.DayKey]usage.Day{}} }

var _ usage.Days = (*Days)(nil)

func (d *Days) AddDay(_ context.Context, k usage.DayKey, v usage.Day, _ time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.rows[k]
	r.Runs += v.Runs
	r.Turns += v.Turns
	r.InputTokens += v.InputTokens
	r.OutputTokens += v.OutputTokens
	r.CacheRead += v.CacheRead
	r.CacheWrite += v.CacheWrite
	r.CacheWrite1h += v.CacheWrite1h
	r.CostMicroUSD += v.CostMicroUSD
	r.CreditsList += v.CreditsList
	r.CreditsCharged += v.CreditsCharged
	d.rows[k] = r
	return nil
}

func (d *Days) DayRows(_ context.Context, day string) ([]usage.DayRow, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []usage.DayRow
	for k, v := range d.rows {
		if k.Day == day {
			out = append(out, usage.DayRow{DayKey: k, Day: v})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].DayKey, out[j].DayKey
		return a.Provider+"|"+a.Agent+"|"+a.Source < b.Provider+"|"+b.Agent+"|"+b.Source
	})
	return out, nil
}
