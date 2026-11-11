package schedule_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/schedule"
	"github.com/kiff/agents/schedule/scheduletest"
)

// failingFixture is a fixture whose runs fail before any turn, every 15
// minutes, until works is set.
func failingFixture(t *testing.T) (*fixture, *bool, *bytes.Buffer) {
	t.Helper()
	f := newFixture()
	works := false
	f.sv.Start = func(_ context.Context, fi schedule.Firing) error {
		if !works {
			return schedule.ErrUnworked
		}
		f.started = append(f.started, fi)
		return nil
	}
	var logs bytes.Buffer
	f.sv.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	s := scheduletest.Schedule("t1", "inbox")
	s.Cron, s.TimeZone = "*/15 * * * *", "UTC"
	if err := f.store.Put(ctx, s); err != nil {
		t.Fatal(err)
	}
	return f, &works, &logs
}

func quarter(n int) time.Time {
	return time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC).Add(time.Duration(n) * 15 * time.Minute)
}

func last(t *testing.T, f *fixture) schedule.Fired {
	t.Helper()
	h, err := f.history.Firings(ctx, "t1", "inbox", 1)
	if err != nil || len(h) != 1 {
		t.Fatalf("history %v %v", h, err)
	}
	return h[0]
}

// Three failures in a row halve its rate, never more often than hourly,
// and the skipped firings aren't failures; six pause it, said in the log.
func TestAFailingScheduleSlowsThenPauses(t *testing.T) {
	f, _, logs := failingFixture(t)
	n := 0
	fire := func() schedule.Fired {
		t.Helper()
		_ = f.sv.Fire(ctx, "t1", "inbox", quarter(n))
		n++
		return last(t, f)
	}
	for i := 0; i < schedule.SlowAfter; i++ {
		if got := fire(); got.Outcome != schedule.FiredUnworked {
			t.Fatalf("firing %d: %s", i, got.Outcome)
		}
	}
	// Every 15 minutes, so slowed to hourly: three skipped, the fourth runs.
	for failures := schedule.SlowAfter; failures < schedule.PauseAfter; failures++ {
		for i := 0; i < 3; i++ {
			if got := fire(); got.Outcome != schedule.FiredSlowed {
				t.Fatalf("after %d failures, firing %d: %s, want slowed", failures, n-1, got.Outcome)
			}
		}
		if got := fire(); got.Outcome != schedule.FiredUnworked {
			t.Fatalf("after %d failures, the hourly firing: %s", failures, got.Outcome)
		}
		if s, _ := f.store.Get(ctx, "t1", "inbox"); s.Failing.Firings != failures+1 {
			t.Fatalf("slowed firings were counted: %+v", s.Failing)
		}
	}
	s, _ := f.store.Get(ctx, "t1", "inbox")
	if s.Enabled || s.Failing == nil || !s.Failing.Paused || s.Failing.Firings != schedule.PauseAfter {
		t.Fatalf("after %d failures: enabled %v failing %+v", schedule.PauseAfter, s.Enabled, s.Failing)
	}
	if !strings.Contains(logs.String(), `"msg":"schedule_paused"`) || !strings.Contains(logs.String(), `"msg":"schedule_slowed"`) {
		t.Fatalf("log: %s", logs.String())
	}
	if strings.Contains(logs.String(), "Reconcile") {
		t.Fatal("the log carries the schedule's input")
	}
	// Paused: its next firing starts nothing.
	if err := f.sv.Fire(ctx, "t1", "inbox", quarter(n)); err != nil {
		t.Fatal(err)
	}
	if len(f.started) != 0 {
		t.Fatalf("a paused schedule ran: %+v", f.started)
	}
}

// One run that works resets it: the next firing runs at the full rate.
func TestASuccessResetsTheStreak(t *testing.T) {
	f, works, _ := failingFixture(t)
	for i := 0; i < schedule.SlowAfter; i++ {
		_ = f.sv.Fire(ctx, "t1", "inbox", quarter(i))
	}
	*works = true
	_ = f.sv.Fire(ctx, "t1", "inbox", quarter(7)) // an hour after the last failure: not slowed
	if s, _ := f.store.Get(ctx, "t1", "inbox"); s.Failing != nil || len(f.started) != 1 {
		t.Fatalf("after a run that worked: failing %+v, started %d", s.Failing, len(f.started))
	}
	if err := f.sv.Fire(ctx, "t1", "inbox", quarter(8)); err != nil || last(t, f).Outcome != schedule.FiredRun {
		t.Fatalf("the next firing: %v %s", err, last(t, f).Outcome)
	}
}

// Quiet firings, credit waits and halts never count as failures.
func TestQuietFiringsNeverSlowIt(t *testing.T) {
	f := newFixture()
	s := scheduletest.Schedule("t1", "inbox")
	s.Cron, s.TimeZone = "*/15 * * * *", "UTC"
	s.AppCheck = &schedule.Check{Tool: "gmail__list", Items: "data", Cursor: "created", Last: "100"}
	if err := f.store.Put(ctx, s); err != nil {
		t.Fatal(err)
	}
	f.sv.Probe = func(context.Context, string, string, schedule.Check) (string, error) { return `{"data":[]}`, nil }
	for i := 0; i < 2*schedule.PauseAfter; i++ {
		if err := f.sv.Fire(ctx, "t1", "inbox", quarter(i)); err != nil {
			t.Fatal(err)
		}
		if got := last(t, f).Outcome; got != schedule.FiredQuiet {
			t.Fatalf("firing %d: %s", i, got)
		}
	}
	if got, _ := f.store.Get(ctx, "t1", "inbox"); got.Failing != nil || !got.Enabled {
		t.Fatalf("quiet firings counted: %+v", got)
	}
}

// A follow-up fires once: never slowed, never paused by the streak.
func TestAFollowUpIsNeverSlowed(t *testing.T) {
	s := schedule.Schedule{Kind: schedule.KindOnce, At: quarter(1), Enabled: true,
		Failing: &schedule.Failing{Firings: schedule.PauseAfter, Last: quarter(0)}}
	if s.Next(quarter(0)).IsZero() {
		t.Fatal("the fixture's once schedule doesn't fire")
	}
	f := newFixture()
	s.Tenant, s.ID, s.Agent, s.Input = "t1", "later", "payouts", "x"
	if err := f.store.Put(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := f.sv.Fire(ctx, "t1", "later", quarter(1)); err != nil {
		t.Fatal(err)
	}
	if len(f.started) != 1 {
		t.Fatalf("a follow-up was slowed: %d started", len(f.started))
	}
}

// An agent's scheduled runs stop at the day's ceiling: its schedules'
// firings that day plus the follow-ups' room, never above CeilingMax. The
// stop is said once, and the next day starts anew.
func TestTheDailyCeilingStopsTheRunPastIt(t *testing.T) {
	f := newFixture()
	var logs bytes.Buffer
	f.sv.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	s := scheduletest.Schedule("t1", "inbox")
	s.Cron, s.TimeZone = "0 9 * * *", "UTC" // once a day: a ceiling of 1 + CeilingFollowUps
	if err := f.store.Put(ctx, s); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	ceiling := schedule.CeilingOf([]schedule.Schedule{s}, day, day.AddDate(0, 0, 1))
	if ceiling != 1+schedule.CeilingFollowUps {
		t.Fatalf("ceiling %d", ceiling)
	}
	// The day's runs so far, as its history keeps them (follow-ups, say).
	for i := 0; i < ceiling; i++ {
		if err := f.history.Add(ctx, "t1", "inbox", schedule.Fired{Due: day.Add(time.Duration(i) * time.Minute), Outcome: schedule.FiredRun}); err != nil {
			t.Fatal(err)
		}
	}
	at9 := day.Add(9 * time.Hour)
	if err := f.sv.Fire(ctx, "t1", "inbox", at9); err != nil {
		t.Fatal(err)
	}
	if len(f.started) != 0 || last(t, f).Outcome != schedule.FiredCeiling {
		t.Fatalf("past the ceiling: started %d, %s", len(f.started), last(t, f).Outcome)
	}
	got, _ := f.store.Get(ctx, "t1", "inbox")
	if got.Ceiling == nil || got.Ceiling.Day != "2026-11-11" || got.Ceiling.Runs != ceiling || got.Failing != nil {
		t.Fatalf("the schedule: ceiling %+v failing %+v", got.Ceiling, got.Failing)
	}
	// Said once a day: a repeated delivery logs nothing more.
	_ = f.sv.Fire(ctx, "t1", "inbox", at9)
	if c := strings.Count(logs.String(), `"msg":"run_ceiling"`); c != 1 {
		t.Fatalf("said %d times: %s", c, logs.String())
	}
	// The next day starts anew.
	if err := f.sv.Fire(ctx, "t1", "inbox", at9.AddDate(0, 0, 1)); err != nil || len(f.started) != 1 {
		t.Fatalf("the next day: %v, started %d", err, len(f.started))
	}
}

// The ceiling never passes CeilingMax, however many schedules an agent has.
func TestTheCeilingIsBounded(t *testing.T) {
	day := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	var many []schedule.Schedule
	for i := 0; i < 5; i++ {
		s := scheduletest.Schedule("t1", "s")
		s.Cron, s.TimeZone = "*/15 * * * *", "UTC"
		many = append(many, s)
	}
	if got := schedule.CeilingOf(many, day, day.AddDate(0, 0, 1)); got != schedule.CeilingMax {
		t.Fatalf("ceiling %d, want %d", got, schedule.CeilingMax)
	}
	off := many[0]
	off.Enabled = false
	if got := schedule.CeilingOf([]schedule.Schedule{off}, day, day.AddDate(0, 0, 1)); got != schedule.CeilingFollowUps {
		t.Fatalf("an off schedule's ceiling %d", got)
	}
}

// Half rate is twice the step, not the step: a daily schedule that keeps
// failing runs every other day, where the hour's floor can't hide it.
func TestADailyFailingScheduleRunsEveryOtherDay(t *testing.T) {
	f, _, _ := failingFixture(t)
	s, _ := f.store.Get(ctx, "t1", "inbox")
	s.Cron = "0 9 * * *"
	if err := f.store.Put(ctx, s); err != nil {
		t.Fatal(err)
	}
	day := func(n int) time.Time { return time.Date(2026, 11, 2+n, 9, 0, 0, 0, time.UTC) }
	for n := 0; n < schedule.SlowAfter; n++ {
		_ = f.sv.Fire(ctx, "t1", "inbox", day(n))
	}
	want := []string{schedule.FiredSlowed, schedule.FiredUnworked, schedule.FiredSlowed, schedule.FiredUnworked}
	for i, w := range want {
		_ = f.sv.Fire(ctx, "t1", "inbox", day(schedule.SlowAfter+i))
		if got := last(t, f).Outcome; got != w {
			t.Fatalf("day %d after %d failures: %s, want %s", schedule.SlowAfter+i, schedule.SlowAfter, got, w)
		}
	}
}

// Only the agent's own schedules' runs count toward its ceiling: another
// agent's busy day in the same account stops nothing of this one's.
func TestAnotherAgentsRunsDontCountTowardTheCeiling(t *testing.T) {
	f := newFixture()
	mine := scheduletest.Schedule("t1", "inbox")
	mine.Cron, mine.TimeZone = "0 9 * * *", "UTC"
	theirs := scheduletest.Schedule("t1", "refunds-daily")
	theirs.Agent, theirs.Cron, theirs.TimeZone = "refunds", "0 9 * * *", "UTC"
	for _, s := range []schedule.Schedule{mine, theirs} {
		if err := f.store.Put(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	day := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	for i := 0; i < schedule.CeilingMax; i++ {
		if err := f.history.Add(ctx, "t1", "refunds-daily", schedule.Fired{Due: day.Add(time.Duration(i) * time.Minute), Outcome: schedule.FiredRun}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.sv.Fire(ctx, "t1", "inbox", day.Add(9*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(f.started) != 1 || last(t, f).Outcome != schedule.FiredRun {
		t.Fatalf("stopped by another agent's runs: started %d, %s", len(f.started), last(t, f).Outcome)
	}
}
