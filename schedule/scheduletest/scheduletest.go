// Package scheduletest holds the checks every schedule.Schedules store must
// pass. Each store's tests call Run with a function that returns an empty
// store.
package scheduletest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/schedule"
)

// Schedule returns a valid schedule for tenant and id.
func Schedule(tenant, id string) schedule.Schedule {
	return schedule.Schedule{Tenant: tenant, ID: id, Agent: "payouts", Cron: "0 9 * * 1-5",
		TimeZone: "Europe/Paris", Input: "Reconcile yesterday's payouts <and> report.", Enabled: true}
}

// Run runs every check against the stores newStore returns, one per check.
func Run(t *testing.T, newStore func(t *testing.T) schedule.Schedules) {
	ctx := context.Background()
	put := func(t *testing.T, s schedule.Schedules, sc schedule.Schedule) {
		t.Helper()
		if err := s.Put(ctx, sc); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("TheRulesAreKept", func(t *testing.T) {
		s := newStore(t)
		want := Schedule("t1", "morning")
		want.Missed, want.Busy = schedule.MissedSkip, schedule.BusySkip
		put(t, s, want)
		if got, err := s.Get(ctx, "t1", "morning"); err != nil || got != want {
			t.Fatalf("got %+v (err %v)", got, err)
		}
	})

	t.Run("HistoryKeepsEachFiringOnceNewestFirst", func(t *testing.T) {
		h, ok := newStore(t).(schedule.History)
		if !ok {
			t.Skip("this store keeps no history")
		}
		day := time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)
		for i, o := range []string{schedule.FiredRun, schedule.FiredMissed, schedule.FiredBusy} {
			due := day.Add(time.Duration(i) * 24 * time.Hour)
			if err := h.Add(ctx, "t1", "morning", schedule.Fired{Due: due, At: due.Add(time.Minute), Outcome: o}); err != nil {
				t.Fatal(err)
			}
		}
		// The first firing again (a repeated delivery): it replaces itself.
		if err := h.Add(ctx, "t1", "morning", schedule.Fired{Due: day, At: day.Add(2 * time.Minute), Outcome: schedule.FiredRun, Run: "r1"}); err != nil {
			t.Fatal(err)
		}
		if err := h.Add(ctx, "t2", "morning", schedule.Fired{Due: day, At: day, Outcome: schedule.FiredRun}); err != nil {
			t.Fatal(err)
		}
		got, err := h.Firings(ctx, "t1", "morning", 10)
		if err != nil || len(got) != 3 {
			t.Fatalf("%d firings (%v)", len(got), err)
		}
		if got[0].Outcome != schedule.FiredBusy || got[2].Outcome != schedule.FiredRun || got[2].Run != "r1" || !got[2].Due.Equal(day) {
			t.Fatalf("history %+v", got)
		}
		if two, err := h.Firings(ctx, "t1", "morning", 2); err != nil || len(two) != 2 || two[0].Outcome != schedule.FiredBusy {
			t.Fatalf("limited: %+v (%v)", two, err)
		}
	})

	t.Run("GetReturnsWhatWasPut", func(t *testing.T) {
		s := newStore(t)
		want := Schedule("t1", "morning")
		want.Input = strings.Repeat("<&>", schedule.MaxInput/3)
		put(t, s, want)
		if got, err := s.Get(ctx, "t1", "morning"); err != nil || got != want {
			t.Fatalf("got %+v (err %v)\nwant %+v", got, err, want)
		}
	})

	t.Run("PutReplaces", func(t *testing.T) {
		s := newStore(t)
		sc := Schedule("t1", "morning")
		put(t, s, sc)
		sc.Cron, sc.Enabled, sc.TimeZone = "30 7 * * *", false, ""
		put(t, s, sc)
		if got, err := s.Get(ctx, "t1", "morning"); err != nil || got != sc {
			t.Fatalf("got %+v (err %v), want the replacement %+v", got, err, sc)
		}
	})

	t.Run("MissingIsNotFound", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.Get(ctx, "t1", "nothing"); !errors.Is(err, schedule.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("TenantsAreApart", func(t *testing.T) {
		s := newStore(t)
		mine, theirs := Schedule("t1", "morning"), Schedule("t2", "morning")
		theirs.Input = "Another tenant's job."
		put(t, s, mine)
		put(t, s, theirs)
		if got, err := s.Get(ctx, "t1", "morning"); err != nil || got != mine {
			t.Fatalf("t1 got %+v (err %v), want its own", got, err)
		}
		if _, err := s.Get(ctx, "t3", "morning"); !errors.Is(err, schedule.ErrNotFound) {
			t.Fatalf("a tenant with no schedules found one: %v", err)
		}
		if err := s.Delete(ctx, "t2", "morning"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(ctx, "t1", "morning"); err != nil {
			t.Fatalf("deleting t2's schedule removed t1's: %v", err)
		}
	})

	t.Run("ListIsTheTenantsByID", func(t *testing.T) {
		s := newStore(t)
		// Enough schedules that a store returning them unsorted is caught.
		for _, id := range []string{"weekly", "daily", "morning", "close", "audit", "payroll", "taxes", "evening"} {
			put(t, s, Schedule("t1", id))
		}
		put(t, s, Schedule("t2", "other"))
		list, err := s.List(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, sc := range list {
			if sc.Tenant != "t1" || sc.Cron == "" {
				t.Fatalf("t1's list holds %+v", sc)
			}
			ids = append(ids, sc.ID)
		}
		const want = "audit,close,daily,evening,morning,payroll,taxes,weekly"
		if got := strings.Join(ids, ","); got != want {
			t.Fatalf("listed %s, want %s", got, want)
		}
	})

	t.Run("DeleteIsSafeToRepeat", func(t *testing.T) {
		s := newStore(t)
		put(t, s, Schedule("t1", "morning"))
		for i := range 2 {
			if err := s.Delete(ctx, "t1", "morning"); err != nil {
				t.Fatalf("delete %d: %v", i+1, err)
			}
		}
		if _, err := s.Get(ctx, "t1", "morning"); !errors.Is(err, schedule.ErrNotFound) {
			t.Fatalf("got %v after delete, want ErrNotFound", err)
		}
	})
}
