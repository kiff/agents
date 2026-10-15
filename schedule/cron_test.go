package schedule_test

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/kiff/agents/schedule"
)

func TestParseCronRefuses(t *testing.T) {
	for _, expr := range []string{
		"", "* * * *", "* * * * * *",
		"60 * * * *", "* 24 * * *", "* * 0 * *", "* * 32 * *", "* * * 0 *", "* * * 13 *", "* * * * 8",
		"*/0 * * * *", "5-1 * * * *", "a * * * *", "1- * * * *", "-1 * * * *", "1,,2 * * * *", "MON * * * *",
		"0 9 1 * 1",    // both day fields: POSIX fires on either, EventBridge can't
		"0 9 1-31 * 1", // the same, though the days of the month are all of them
	} {
		if _, err := schedule.ParseCron(expr); err == nil {
			t.Errorf("%q: accepted", expr)
		}
	}
}

func TestEventBridge(t *testing.T) {
	for expr, want := range map[string]string{
		"0 9 * * 1-5":       "cron(0 9 ? * 2,3,4,5,6 *)",
		"*/15 * * * *":      "cron(0,15,30,45 * * * ? *)",
		"0/20 8-18/2 * * *": "cron(0,20,40 8,10,12,14,16,18 * * ? *)",
		"0 8 1,15 * *":      "cron(0 8 1,15 * ? *)",
		"0 0 * * 0":         "cron(0 0 ? * 1 *)",
		"0 0 * * 7":         "cron(0 0 ? * 1 *)",
		"0 0 * * 0-6":       "cron(0 0 * * ? *)",
		"30 7 * 1,7 *":      "cron(30 7 * 1,7 ? *)",
		"0 9 * * *":         "cron(0 9 * * ? *)",
	} {
		c, err := schedule.ParseCron(expr)
		if err != nil {
			t.Fatalf("%q: %v", expr, err)
		}
		if got := c.EventBridge(); got != want {
			t.Errorf("%q: got %s, want %s", expr, got, want)
		}
	}
}

func TestMinGap(t *testing.T) {
	for expr, want := range map[string]int{
		"*/15 * * * *": 15, "*/10 * * * *": 10, "0,50 * * * *": 10, "5 * * * *": 60,
		"0,30 * * * *": 30, "* * * * *": 1, "0/20 * * * *": 20, "10,40 9 * * 1": 30,
	} {
		c, err := schedule.ParseCron(expr)
		if err != nil {
			t.Fatalf("%q: %v", expr, err)
		}
		if got := c.MinGap(); got != time.Duration(want)*time.Minute {
			t.Errorf("%q: got %v, want %d minutes", expr, got, want)
		}
	}
}

func TestNext(t *testing.T) {
	paris, _ := time.LoadLocation("Europe/Paris")
	cases := []struct {
		expr  string
		after time.Time
		want  time.Time
	}{
		// Friday after nine: the next weekday is Monday.
		{"0 9 * * 1-5", time.Date(2026, 10, 16, 10, 0, 0, 0, paris), time.Date(2026, 10, 19, 9, 0, 0, 0, paris)},
		// Strictly after: a firing at that very minute is not the next one.
		{"0 9 * * *", time.Date(2026, 10, 16, 9, 0, 0, 0, paris), time.Date(2026, 10, 17, 9, 0, 0, 0, paris)},
		// 02:30 doesn't exist on 28 March 2027 in Paris: that firing is skipped.
		{"30 2 * * *", time.Date(2027, 3, 27, 3, 0, 0, 0, paris), time.Date(2027, 3, 29, 2, 30, 0, 0, paris)},
		{"0 0 29 2 *", time.Date(2026, 10, 15, 0, 0, 0, 0, paris), time.Date(2028, 2, 29, 0, 0, 0, 0, paris)},
		{"0 0 30 2 *", time.Date(2026, 10, 15, 0, 0, 0, 0, paris), time.Time{}},
		// The time given is read in the schedule's zone: 07:30 UTC is 09:30 in Paris.
		{"0 9 * * *", time.Date(2026, 10, 16, 7, 30, 0, 0, time.UTC), time.Date(2026, 10, 17, 9, 0, 0, 0, paris)},
	}
	for _, c := range cases {
		cr, err := schedule.ParseCron(c.expr)
		if err != nil {
			t.Fatal(err)
		}
		if got := cr.Next(c.after, paris); !got.Equal(c.want) {
			t.Errorf("%q after %v: got %v, want %v", c.expr, c.after, got, c.want)
		}
	}
}

// Next agrees with trying every minute, in zones with and without daylight
// saving, around their changes.
func TestNextMatchesEveryMinute(t *testing.T) {
	exprs := []string{"0 9 * * 1-5", "*/15 * * * *", "30 2 * * *", "0/20 8-18/2 * * *", "0 8 1,15 * *",
		"45 23 * * 0", "0 0 * * 3", "15 1,2,3 * * *", "0 12 31 * *"}
	var zones []*time.Location
	for _, name := range []string{"UTC", "Europe/Paris", "America/New_York", "Australia/Lord_Howe", "Asia/Kolkata"} {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatal(err)
		}
		zones = append(zones, loc)
	}
	rng := rand.New(rand.NewPCG(1, 2))
	starts := []time.Time{
		time.Date(2027, 3, 27, 22, 0, 0, 0, time.UTC),  // around Europe's spring change
		time.Date(2026, 10, 24, 22, 0, 0, 0, time.UTC), // around Europe's autumn change
		time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC),   // around New York's autumn change
	}
	for range 20 {
		starts = append(starts, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(rng.Int64N(int64(2*365*24*time.Hour)))))
	}
	for _, expr := range exprs {
		c, err := schedule.ParseCron(expr)
		if err != nil {
			t.Fatal(err)
		}
		for _, loc := range zones {
			for _, start := range starts {
				got := c.Next(start, loc)
				if want := everyMinute(t, expr, start, loc); !got.Equal(want) {
					t.Fatalf("%q in %s after %v: got %v, want %v", expr, loc, start, got, want)
				}
			}
		}
	}
}

// everyMinute finds the next firing by trying each minute for 70 days: past the gap between two 31sts.
func everyMinute(t *testing.T, expr string, after time.Time, loc *time.Location) time.Time {
	t.Helper()
	c, _ := schedule.ParseCron(expr)
	at := after.Truncate(time.Minute).Add(time.Minute)
	for end := at.Add(70 * 24 * time.Hour); at.Before(end); at = at.Add(time.Minute) {
		l := at.In(loc)
		// A minute matches when its local time is one the cron names and
		// it's the first instant with that local time (a repeated hour fires
		// once, like time.Date picks one).
		if c.Matches(l) && time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), 0, 0, loc).Equal(at) {
			return at
		}
	}
	t.Fatalf("%q: no firing in 70 days after %v", expr, after)
	return time.Time{}
}
