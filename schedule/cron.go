package schedule

import (
	"errors"
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed five-field POSIX cron expression: minute, hour, day of
// the month, month, day of the week (0 or 7 is Sunday).
//
// Only what EventBridge Scheduler can run exactly is accepted. When neither
// day field is "*", POSIX fires on a day that matches either, and EventBridge
// can't say "either": one of the two must be "*".
type Cron struct {
	minute, hour, dom, month, dow uint64 // bit n set: fires at n
}

// field is one cron field's range.
type field struct {
	name     string
	min, max int
}

var fields = [5]field{{"minute", 0, 59}, {"hour", 0, 23}, {"day of the month", 1, 31}, {"month", 1, 12}, {"day of the week", 0, 7}}

// ParseCron parses a five-field POSIX cron expression. Each field is "*" or a
// list of values, ranges ("1-5") and steps ("*/15", "0/20", "8-18/2").
func ParseCron(expr string) (Cron, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return Cron{}, fmt.Errorf("schedule: %q is not a five-field cron expression", expr)
	}
	var sets [5]uint64
	for i, p := range parts {
		set, err := parseField(p, fields[i])
		if err != nil {
			return Cron{}, err
		}
		sets[i] = set
	}
	c := Cron{minute: sets[0], hour: sets[1], dom: sets[2], month: sets[3], dow: sets[4]}
	if c.dow&(1<<7) != 0 { // 7 is Sunday too
		c.dow = c.dow&^(1<<7) | 1
	}
	if parts[2] != "*" && parts[4] != "*" {
		return Cron{}, errors.New(`schedule: the day of the month or the day of the week must be "*"`)
	}
	return c, nil
}

func parseField(s string, f field) (uint64, error) {
	bad := func() (uint64, error) {
		return 0, fmt.Errorf("schedule: %q is not a %s (%d to %d)", s, f.name, f.min, f.max)
	}
	var set uint64
	for _, part := range strings.Split(s, ",") {
		rng, stepText, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepText)
			if err != nil || n < 1 {
				return bad()
			}
			step = n
		}
		lo, hi := f.min, f.max
		switch {
		case rng == "*":
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err1, err2 error
			lo, err1 = strconv.Atoi(a)
			hi, err2 = strconv.Atoi(b)
			if err1 != nil || err2 != nil || lo > hi {
				return bad()
			}
		default:
			n, err := strconv.Atoi(rng)
			if err != nil {
				return bad()
			}
			lo = n
			if !hasStep { // "5" is just 5; "5/15" is 5 to the end, every 15
				hi = n
			}
		}
		if lo < f.min || hi > f.max {
			return bad()
		}
		for v := lo; v <= hi; v += step {
			set |= 1 << v
		}
	}
	return set, nil
}

func (c Cron) allWeekdays() bool { return c.dow == bitsFrom(0, 6) }

func bitsFrom(lo, hi int) uint64 { return (1<<(hi+1) - 1) &^ (1<<lo - 1) }

// MinGap is the shortest time between two firings, as tafs bounds it: the
// gaps between the minutes it fires at, including the one across the hour.
// A cron that fires once an hour or less often has a gap of an hour.
func (c Cron) MinGap() time.Duration {
	mins := values(c.minute)
	gap := 60
	for i := 1; i < len(mins); i++ {
		gap = min(gap, mins[i]-mins[i-1])
	}
	if len(mins) > 1 {
		gap = min(gap, 60-mins[len(mins)-1]+mins[0])
	}
	return time.Duration(gap) * time.Minute
}

func values(set uint64) []int {
	var out []int
	for set != 0 {
		v := bits.TrailingZeros64(set)
		out = append(out, v)
		set &^= 1 << v
	}
	return out
}

// Matches reports whether the cron names t's minute, read on t's own clock.
func (c Cron) Matches(t time.Time) bool {
	return c.minute&(1<<t.Minute()) != 0 && c.hour&(1<<t.Hour()) != 0 && c.month&(1<<int(t.Month())) != 0 &&
		c.dom&(1<<t.Day()) != 0 && c.dow&(1<<int(t.Weekday())) != 0
}

// maxYears bounds how far Next looks: far enough for any 29 February.
const maxYears = 8

// Next is the first firing strictly after t, in loc. A firing at a time loc
// skips (the hour clocks go forward) doesn't happen. It returns the zero time
// when the cron never fires, such as on 30 February.
func (c Cron) Next(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	end := day.AddDate(maxYears, 0, 0)
	for ; day.Before(end); day = day.AddDate(0, 0, 1) {
		if c.month&(1<<int(day.Month())) == 0 || c.dom&(1<<day.Day()) == 0 || c.dow&(1<<int(day.Weekday())) == 0 {
			continue
		}
		for _, h := range values(c.hour) {
			for _, m := range values(c.minute) {
				at := time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, loc)
				if at.Hour() != h || at.Minute() != m {
					continue // a skipped local time
				}
				if at.After(t) {
					return at
				}
			}
		}
	}
	return time.Time{}
}

// EventBridge is the cron as EventBridge Scheduler writes it:
// cron(minutes hours day-of-month month day-of-week year), where days of the
// week run 1 (Sunday) to 7 and one of the two day fields is "?".
func (c Cron) EventBridge() string {
	dom, dow := list(c.dom, bitsFrom(1, 31)), "?"
	if dom == "*" && !c.allWeekdays() {
		dom, dow = "?", list(c.dow<<1, 0) // POSIX 0 (Sunday) is EventBridge's 1
	}
	return fmt.Sprintf("cron(%s %s %s %s %s *)", list(c.minute, bitsFrom(0, 59)), list(c.hour, bitsFrom(0, 23)),
		dom, list(c.month, bitsFrom(1, 12)), dow)
}

// list writes a set as "*" when it is all, otherwise as values.
func list(set, all uint64) string {
	if all != 0 && set == all {
		return "*"
	}
	vs := values(set)
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = strconv.Itoa(v)
	}
	return strings.Join(out, ",")
}
