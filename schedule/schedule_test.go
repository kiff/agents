package schedule_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/schedule"
	"github.com/kiff/agents/schedule/scheduletest"
	"github.com/kiff/agents/store/memory"
)

var ctx = context.Background()

func TestCheckRefuses(t *testing.T) {
	cases := map[string]func(*schedule.Schedule){
		"no tenant":            func(s *schedule.Schedule) { s.Tenant = "" },
		"a long tenant":        func(s *schedule.Schedule) { s.Tenant = strings.Repeat("t", 257) },
		"an id with a slash":   func(s *schedule.Schedule) { s.ID = "a/b" },
		"no agent":             func(s *schedule.Schedule) { s.Agent = "" },
		"no input":             func(s *schedule.Schedule) { s.Input = "" },
		"a long input":         func(s *schedule.Schedule) { s.Input = strings.Repeat("x", schedule.MaxInput+1) },
		"a bad cron":           func(s *schedule.Schedule) { s.Cron = "0 9 * *" },
		"every ten minutes":    func(s *schedule.Schedule) { s.Cron = "*/10 * * * *" },
		"twice across an hour": func(s *schedule.Schedule) { s.Cron = "0,55 * * * *" },
		"an unknown zone":      func(s *schedule.Schedule) { s.TimeZone = "Mars/Olympus" },
		"the server's zone":    func(s *schedule.Schedule) { s.TimeZone = "Local" },
	}
	if err := scheduletest.Schedule("t1", "morning").Check(); err != nil {
		t.Fatalf("the valid schedule is refused: %v", err)
	}
	for name, change := range cases {
		s := scheduletest.Schedule("t1", "morning")
		change(&s)
		if err := s.Check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNextIsInTheSchedulesZoneAndOnlyWhenOn(t *testing.T) {
	s := scheduletest.Schedule("t1", "morning") // 09:00 on weekdays, Paris
	after := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	if got, want := s.Next(after), time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	s.TimeZone = ""
	if got, want := s.Next(after), time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("with no zone got %v, want %v (UTC)", got, want)
	}
	s.Enabled = false
	if got := s.Next(after); !got.IsZero() {
		t.Fatalf("a schedule that is off fires at %v", got)
	}
}

func TestFiringKey(t *testing.T) {
	due := time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)
	f := schedule.Firing{Tenant: "t1", Schedule: "morning", Agent: "payouts", Input: "go", Due: due}
	again := f
	again.Due = due.In(time.FixedZone("", 7200))
	again.Input, again.Agent = "changed since", "chargebacks"
	if f.Key() != again.Key() {
		t.Fatal("two deliveries of one firing have different keys")
	}
	for name, other := range map[string]schedule.Firing{
		"another time":     {Tenant: "t1", Schedule: "morning", Due: due.Add(time.Minute)},
		"another schedule": {Tenant: "t1", Schedule: "evening", Due: due},
		"another tenant":   {Tenant: "t2", Schedule: "morning", Due: due},
		"a shifted split":  {Tenant: "t1|5:m", Schedule: "orning", Due: due},
	} {
		if other.Key() == f.Key() {
			t.Errorf("%s: same key", name)
		}
	}
}

// scheduler records what it was asked.
type scheduler struct {
	set     map[[2]string]schedule.Schedule
	removed []string
	fail    error
}

func (s *scheduler) Set(_ context.Context, sc schedule.Schedule) error {
	if s.fail != nil {
		return s.fail
	}
	s.set[[2]string{sc.Tenant, sc.ID}] = sc
	return nil
}

func (s *scheduler) Remove(_ context.Context, tenant, id string) error {
	if s.fail != nil {
		return s.fail
	}
	delete(s.set, [2]string{tenant, id})
	s.removed = append(s.removed, tenant+"/"+id)
	return nil
}

type fixture struct {
	sv      *schedule.Service
	store   *memory.Schedules
	sched   *scheduler
	agents  map[[2]string]bool
	started []schedule.Firing
}

func newFixture() *fixture {
	f := &fixture{store: memory.NewSchedules(), sched: &scheduler{set: map[[2]string]schedule.Schedule{}},
		agents: map[[2]string]bool{{"t1", "payouts"}: true, {"t1", "refunds"}: true, {"t2", "payouts"}: true}}
	f.sv = &schedule.Service{Store: f.store, Scheduler: f.sched,
		Live: func(_ context.Context, tenant, agent string) (bool, error) {
			return f.agents[[2]string{tenant, agent}], nil
		},
		Start: func(_ context.Context, fi schedule.Firing) error {
			f.started = append(f.started, fi)
			return nil
		},
	}
	return f
}

func TestPutStoresThenSets(t *testing.T) {
	f := newFixture()
	s := scheduletest.Schedule("t1", "morning")
	if err := f.sv.Put(ctx, s); err != nil {
		t.Fatal(err)
	}
	if got, err := f.store.Get(ctx, "t1", "morning"); err != nil || got != s {
		t.Fatalf("stored %+v (err %v)", got, err)
	}
	if f.sched.set[[2]string{"t1", "morning"}] != s {
		t.Fatal("not set to fire")
	}
}

func TestPutRefusesBeforeStoring(t *testing.T) {
	t.Run("an invalid schedule", func(t *testing.T) {
		f := newFixture()
		s := scheduletest.Schedule("t1", "morning")
		s.Cron = "* * * * *"
		if err := f.sv.Put(ctx, s); err == nil {
			t.Fatal("accepted a schedule that fires every minute")
		}
		if list, _ := f.store.List(ctx, "t1"); len(list) != 0 || len(f.sched.set) != 0 {
			t.Fatal("stored or set a refused schedule")
		}
	})
	t.Run("an agent the tenant doesn't have", func(t *testing.T) {
		f := newFixture()
		s := scheduletest.Schedule("t1", "morning")
		s.Agent = "chargebacks"
		if err := f.sv.Put(ctx, s); !errors.Is(err, schedule.ErrNoAgent) {
			t.Fatalf("got %v, want ErrNoAgent", err)
		}
		if list, _ := f.store.List(ctx, "t1"); len(list) != 0 || len(f.sched.set) != 0 {
			t.Fatal("stored or set a schedule for no agent")
		}
	})
}

func TestPutThatFailsToSetCanBeRetried(t *testing.T) {
	f := newFixture()
	s := scheduletest.Schedule("t1", "morning")
	f.sched.fail = errors.New("throttled")
	if err := f.sv.Put(ctx, s); err == nil {
		t.Fatal("a Put whose Set failed reported success")
	}
	// What is stored is the truth: the owner sees the schedule, and a retry
	// sets it.
	if got, err := f.store.Get(ctx, "t1", "morning"); err != nil || got != s {
		t.Fatalf("after a failed Set, stored %+v (err %v)", got, err)
	}
	f.sched.fail = nil
	if err := f.sv.Put(ctx, s); err != nil || f.sched.set[[2]string{"t1", "morning"}] != s {
		t.Fatalf("the retry: %v", err)
	}
}

func TestDeleteStopsThenForgets(t *testing.T) {
	f := newFixture()
	_ = f.sv.Put(ctx, scheduletest.Schedule("t1", "morning"))
	f.sched.fail = errors.New("throttled")
	if err := f.sv.Delete(ctx, "t1", "morning"); err == nil {
		t.Fatal("a Delete whose Remove failed reported success")
	}
	if _, err := f.store.Get(ctx, "t1", "morning"); err != nil {
		t.Fatal("forgot a schedule that may still fire, so a retry can't find it")
	}
	f.sched.fail = nil
	for range 2 {
		if err := f.sv.Delete(ctx, "t1", "morning"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.Get(ctx, "t1", "morning"); !errors.Is(err, schedule.ErrNotFound) || len(f.sched.set) != 0 {
		t.Fatalf("still stored (%v) or set (%d)", err, len(f.sched.set))
	}
}

func TestDeleteAgentDeletesOnlyItsSchedules(t *testing.T) {
	f := newFixture()
	for _, s := range []schedule.Schedule{
		scheduletest.Schedule("t1", "morning"), scheduletest.Schedule("t1", "evening"),
		scheduletest.Schedule("t2", "morning"),
	} {
		_ = f.sv.Put(ctx, s)
	}
	refunds := scheduletest.Schedule("t1", "refunds-daily")
	refunds.Agent = "refunds"
	_ = f.sv.Put(ctx, refunds)

	if err := f.sv.DeleteAgent(ctx, "t1", "payouts"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.sched.removed, ","); got != "t1/evening,t1/morning" {
		t.Fatalf("removed %s, want t1's payouts schedules", got)
	}
	left, _ := f.store.List(ctx, "t1")
	if len(left) != 1 || left[0].ID != "refunds-daily" {
		t.Fatalf("t1 has %+v left, want refunds-daily", left)
	}
	if _, err := f.store.Get(ctx, "t2", "morning"); err != nil {
		t.Fatal("removed another tenant's schedule")
	}
}

func TestFireStartsTheScheduleAsItIsNow(t *testing.T) {
	f := newFixture()
	s := scheduletest.Schedule("t1", "morning")
	_ = f.sv.Put(ctx, s)
	s.Input = "Changed after it was set."
	_ = f.sv.Put(ctx, s)
	if err := f.sv.Fire(ctx, "t1", "morning", due); err != nil {
		t.Fatal(err)
	}
	want := schedule.Firing{Tenant: "t1", Schedule: "morning", Agent: "payouts", Input: s.Input, Due: due}
	if len(f.started) != 1 || f.started[0] != want {
		t.Fatalf("started %+v, want %+v", f.started, want)
	}
}

// due is a Monday at 09:00 in Paris: a time scheduletest.Schedule fires.
var due = time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)

// A firing the stored schedule doesn't name starts nothing, isn't an error
// the Scheduler would retry, and puts the Scheduler back in line with what is
// stored.
func TestFireHealsWhatTheSchedulerGotWrong(t *testing.T) {
	type check func(t *testing.T, f *fixture)
	setAgain := func(t *testing.T, f *fixture) {
		stored, _ := f.store.Get(ctx, "t1", "morning")
		if f.sched.set[[2]string{"t1", "morning"}] != stored {
			t.Fatal("the Scheduler wasn't set back to what is stored")
		}
	}
	removed := func(t *testing.T, f *fixture) {
		if _, ok := f.sched.set[[2]string{"t1", "morning"}]; ok {
			t.Fatal("still set in the Scheduler")
		}
	}
	for name, c := range map[string]struct {
		change func(f *fixture)
		then   check
	}{
		// A Put stored a new cron, and its Set failed: the old one fires.
		"moved to Fridays": {func(f *fixture) {
			s := scheduletest.Schedule("t1", "morning")
			s.Cron = "0 9 * * 5"
			_ = f.store.Put(ctx, s)
		}, setAgain},
		"moved to another zone": {func(f *fixture) {
			s := scheduletest.Schedule("t1", "morning")
			s.TimeZone = "America/New_York"
			_ = f.store.Put(ctx, s)
		}, setAgain},
		"switched off": {func(f *fixture) {
			s := scheduletest.Schedule("t1", "morning")
			s.Enabled = false
			_ = f.store.Put(ctx, s)
		}, setAgain},
		"no longer stored": {func(f *fixture) { _ = f.store.Delete(ctx, "t1", "morning") }, removed},
		"its agent removed": {func(f *fixture) { delete(f.agents, [2]string{"t1", "payouts"}) }, func(t *testing.T, f *fixture) {
			removed(t, f)
			if _, err := f.store.Get(ctx, "t1", "morning"); !errors.Is(err, schedule.ErrNotFound) {
				t.Fatal("a schedule whose agent is gone is still stored")
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture()
			_ = f.sv.Put(ctx, scheduletest.Schedule("t1", "morning"))
			c.change(f)
			if err := f.sv.Fire(ctx, "t1", "morning", due); err != nil || len(f.started) != 0 {
				t.Fatalf("started %d (err %v)", len(f.started), err)
			}
			c.then(t, f)
		})
	}
}

func TestFireLeavesOtherTenantsAlone(t *testing.T) {
	f := newFixture()
	_ = f.sv.Put(ctx, scheduletest.Schedule("t1", "morning"))
	if err := f.sv.Fire(ctx, "t3", "morning", due); err != nil || len(f.started) != 0 {
		t.Fatalf("started %d (err %v)", len(f.started), err)
	}
	if _, ok := f.sched.set[[2]string{"t1", "morning"}]; !ok {
		t.Fatal("a firing for t3 removed t1's schedule")
	}
}

func TestFireReportsWhatShouldBeRetried(t *testing.T) {
	cases := map[string]func(f *fixture){
		"checking the agent": func(f *fixture) {
			f.sv.Live = func(context.Context, string, string) (bool, error) { return false, errors.New("store down") }
		},
		"starting the run": func(f *fixture) {
			f.sv.Start = func(context.Context, schedule.Firing) error { return errors.New("store down") }
		},
		"setting the Scheduler back": func(f *fixture) {
			s := scheduletest.Schedule("t1", "morning")
			s.Cron = "0 9 * * 5"
			_ = f.store.Put(ctx, s)
			f.sched.fail = errors.New("throttled")
		},
		"removing an orphan": func(f *fixture) {
			_ = f.store.Delete(ctx, "t1", "morning")
			f.sched.fail = errors.New("throttled")
		},
	}
	for name, change := range cases {
		f := newFixture()
		_ = f.sv.Put(ctx, scheduletest.Schedule("t1", "morning"))
		change(f)
		if err := f.sv.Fire(ctx, "t1", "morning", due); err == nil {
			t.Errorf("%s failed, and the firing reported success", name)
		}
	}
}
