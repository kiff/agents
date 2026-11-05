package main

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/kiff/agents/model"
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

// Each message's wait is until the first words after it and before the next
// one; a message Coach answered with no words says so, and messages after
// the chat's end aren't counted.
func TestFirstWordsArePerMessage(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 21, 0, 0, 0, time.UTC)
	at := func(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }
	sent := []time.Time{at(0), at(30), at(60), at(200)}
	texts := []time.Time{at(4.2), at(9), at(75), at(205)}
	got := firstWords(sent, texts, at(100))
	want := []time.Duration{4200 * time.Millisecond, -1, 15 * time.Second}
	if !slices.Equal(got, want) {
		t.Fatalf("waits %v, want %v", got, want)
	}
	med, p90, n := percentiles(got)
	if n != 2 || med != 15*time.Second || p90 != 15*time.Second {
		t.Fatalf("median %v, p90 %v, over %d", med, p90, n)
	}
	waits := make([]time.Duration, 10)
	for i := range waits {
		waits[i] = time.Duration(i+1) * time.Second
	}
	if med, p90, n := percentiles(waits); n != 10 || med != 6*time.Second || p90 != 9*time.Second {
		t.Fatalf("1..10 s: median %v, p90 %v, over %d", med, p90, n)
	}
}

// The clock times a turn's first text only, never its blank deltas, and
// passes every delta on as it came.
func TestWordsClockTimesTheFirstText(t *testing.T) {
	w := &wordsClock{Model: deltas{"", " ", "Miro", " las ejecuciones"}}
	var got []string
	if _, err := w.Turn(context.Background(), model.Request{}, func(s string) { got = append(got, s) }); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Turn(context.Background(), model.Request{}, nil); err != nil {
		t.Fatal(err)
	}
	if len(w.times()) != 2 || !slices.Equal(got, []string{"", " ", "Miro", " las ejecuciones"}) {
		t.Fatalf("times %v, deltas %q", w.times(), got)
	}
	if _, err := (&wordsClock{Model: deltas{"", "  "}}).Turn(context.Background(), model.Request{}, nil); err != nil {
		t.Fatal(err)
	}
}

// deltas is a model whose turn streams these texts.
type deltas []string

func (d deltas) Turn(_ context.Context, _ model.Request, onText func(string)) (model.Reply, error) {
	for _, s := range d {
		if onText != nil {
			onText(s)
		}
	}
	return model.Reply{}, nil
}

// A turn's thinking is its output less what it wrote that shows.
func TestTurnThinkingEstimate(t *testing.T) {
	events := []session.Event{{Kind: session.Usage, Body: json.RawMessage(`{"output_tokens":1475,"cost_micro_usd":30000,"timing":{"ms":13750,"first_ms":3211,"thinking_bytes":811,"tool_input_bytes":1211,"tool_calls":2}}`)}}
	turns := turnTimings(events)
	if len(turns) != 1 || turns[0].output != 1475 || turns[0].thinking() != 1475-1211/4 || turns[0].thinkingBytes != 811 {
		t.Fatalf("turns %+v", turns)
	}
	if s := summarize(turns); s.output != 1475 || s.thinking != turns[0].thinking() {
		t.Fatalf("summary %+v", s)
	}
}
