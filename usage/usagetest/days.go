package usagetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/usage"
)

// RunDays checks the usage.Days newDays makes, one fresh store per test.
func RunDays(t *testing.T, newDays func(*testing.T) usage.Days) {
	ctx := context.Background()
	until := time.Now().Add(usage.DaysKeep)
	k := usage.DayKey{Day: "2026-11-25", Provider: "bedrock", Agent: "taf-1", Source: "chrome"}

	t.Run("a day nobody used has no rows", func(t *testing.T) {
		rows, err := newDays(t).DayRows(ctx, "2026-11-25")
		if err != nil || len(rows) != 0 {
			t.Fatalf("got %v, %v", rows, err)
		}
	})

	t.Run("adds sum on one row, and each key is its own", func(t *testing.T) {
		d := newDays(t)
		for _, v := range []usage.Day{
			{Turns: 2, InputTokens: 10, OutputTokens: 100, CacheRead: 5000, CacheWrite: 300, CacheWrite1h: 7, CostMicroUSD: 12_000},
			{Runs: 1, Turns: 1, OutputTokens: 50, CostMicroUSD: 3_000, CreditsList: 6, CreditsCharged: 6},
			{Runs: 1, CostMicroUSD: 1_000, CreditsList: 2},
		} {
			if err := d.AddDay(ctx, k, v, until); err != nil {
				t.Fatal(err)
			}
		}
		other := k
		other.Source = "phone"
		if err := d.AddDay(ctx, other, usage.Day{Runs: 1, CostMicroUSD: 1}, until); err != nil {
			t.Fatal(err)
		}
		if err := d.AddDay(ctx, usage.DayKey{Day: "2026-11-26", Provider: "bedrock", Agent: "taf-1", Source: "chrome"}, usage.Day{Runs: 9}, until); err != nil {
			t.Fatal(err)
		}
		rows, err := d.DayRows(ctx, "2026-11-25")
		if err != nil || len(rows) != 2 {
			t.Fatalf("rows %v, %v", rows, err)
		}
		by := map[usage.DayKey]usage.Day{}
		for _, r := range rows {
			by[r.DayKey] = r.Day
		}
		want := usage.Day{Runs: 2, Turns: 3, InputTokens: 10, OutputTokens: 150, CacheRead: 5000, CacheWrite: 300, CacheWrite1h: 7, CostMicroUSD: 16_000, CreditsList: 8, CreditsCharged: 6}
		if by[k] != want || by[other] != (usage.Day{Runs: 1, CostMicroUSD: 1}) {
			t.Fatalf("rows %+v", by)
		}
	})

	t.Run("adds made at once lose none", func(t *testing.T) {
		d := newDays(t)
		var wg sync.WaitGroup
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := d.AddDay(ctx, k, usage.Day{Runs: 1, CostMicroUSD: 100}, until); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		rows, err := d.DayRows(ctx, k.Day)
		if err != nil || len(rows) != 1 || rows[0].Runs != 20 || rows[0].CostMicroUSD != 2000 {
			t.Fatalf("rows %+v, %v", rows, err)
		}
	})
}
