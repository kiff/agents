package main

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/kiff/agents/session"
)

// setupTimings writes each of the setup chat's model turns to the
// transcript, never the log: its speed, how long it took, how long before
// its first content, and the cache it read and wrote; then the chat's
// totals. They are what kiff/agents#197 is judged by, before and after.
func (t *test) setupTimings(ctx context.Context) {
	if t.setupSession == "" {
		return
	}
	_, events, err := t.sessions.Read(ctx, t.tenant, t.setupSession)
	if err != nil {
		t.logf("setup timings: %v", err)
		return
	}
	turns := turnTimings(events)
	for i, u := range turns {
		t.logf("setup turn %d: speed %s, %d ms, first %d ms, cache read %d, written %d", i+1, u.speed, u.ms, u.first, u.read, u.written)
	}
	if s := summarize(turns); s.turns > 0 {
		t.logf("setup turns: %d (%d fast), %s in all, first content at %s median, cache read %d, written %d",
			s.turns, s.fast, s.total, s.medianFirst, s.read, s.written)
	}
}

type turnTiming struct {
	speed         string
	ms, first     int64
	read, written int64
}

// turnTimings is each model turn of events, in order.
func turnTimings(events []session.Event) []turnTiming {
	var out []turnTiming
	for _, e := range events {
		// The speed read apart from the body, so a runtime from before speeds measures too.
		var u struct {
			session.UsageBody
			Speed string `json:"speed"`
		}
		if e.Kind != session.Usage || json.Unmarshal(e.Body, &u) != nil {
			continue
		}
		tt := turnTiming{speed: u.Speed, read: u.CacheRead, written: u.CacheWrite + u.CacheWrite1h}
		if tt.speed == "" {
			tt.speed = "standard"
		}
		if u.Timing != nil {
			tt.ms, tt.first = u.Timing.MS, u.Timing.FirstMS
		}
		out = append(out, tt)
	}
	return out
}

type turnSummary struct {
	turns, fast   int
	total         time.Duration
	medianFirst   time.Duration
	read, written int64
}

func summarize(turns []turnTiming) turnSummary {
	var s turnSummary
	var firsts []int64
	for _, u := range turns {
		s.turns++
		if u.speed == "fast" {
			s.fast++
		}
		s.total += time.Duration(u.ms) * time.Millisecond
		s.read += u.read
		s.written += u.written
		firsts = append(firsts, u.first)
	}
	if len(firsts) > 0 {
		slices.Sort(firsts)
		s.medianFirst = time.Duration(firsts[len(firsts)/2]) * time.Millisecond
	}
	return s
}
