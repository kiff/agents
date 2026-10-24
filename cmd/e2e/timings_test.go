package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kiff/agents/session"
)

func TestSetupTimingsAreEachTurnsAndTheirTotals(t *testing.T) {
	usage := func(speed string, ms, first, read, write int64) session.Event {
		b, _ := json.Marshal(struct {
			session.UsageBody
			Speed string `json:"speed,omitempty"`
		}{session.UsageBody{Model: "m", CacheRead: read, CacheWrite: write, Timing: &session.TurnTiming{MS: ms, FirstMS: first}, CostMicroUSD: 1000 * ms}, speed})
		return session.Event{Kind: session.Usage, Body: b}
	}
	events := []session.Event{
		{Kind: session.OwnerMessage, Body: json.RawMessage(`{"text":"hola"}`)},
		usage("fast", 4000, 900, 0, 5000),
		usage("fast", 2000, 500, 5000, 300),
		usage("", 3000, 700, 5300, 0),
	}
	turns := turnTimings(events)
	if len(turns) != 3 || turns[2].speed != "standard" {
		t.Fatalf("turns %+v", turns)
	}
	s := summarize(turns)
	if s.turns != 3 || s.fast != 2 || s.total != 9*time.Second || s.medianFirst != 700*time.Millisecond || s.read != 10300 || s.written != 5300 || s.cost != 9_000_000 || usd(s.cost) != "$9.000" {
		t.Fatalf("summary %+v", s)
	}
}
