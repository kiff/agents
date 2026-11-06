package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
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

// A progress note before the turn's first tool call is its first words: the
// person waiting sees it on the live line, before any text. The note still
// reaches whoever asked for it.
func TestWordsClockTimesAProgressNote(t *testing.T) {
	w := &wordsClock{Model: deltas{"(note) Miro las ejecuciones de Reembolsos", "Listo."}}
	var notes, text []string
	if _, err := w.Turn(context.Background(), model.Request{OnUpdate: func(s string) { notes = append(notes, s) }},
		func(s string) { text = append(text, s) }); err != nil {
		t.Fatal(err)
	}
	if len(w.times()) != 1 || !slices.Equal(notes, []string{"Miro las ejecuciones de Reembolsos"}) || !slices.Equal(text, []string{"Listo."}) {
		t.Fatalf("times %v, notes %q, text %q", w.times(), notes, text)
	}
}

// deltas is a model whose turn streams these texts; one that starts "(note) "
// is a progress note instead.
type deltas []string

func (d deltas) Turn(_ context.Context, req model.Request, onText func(string)) (model.Reply, error) {
	for _, s := range d {
		if note, ok := strings.CutPrefix(s, "(note) "); ok {
			if req.OnUpdate != nil {
				req.OnUpdate(note)
			}
		} else if onText != nil {
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

// Setup's progress notes are kept as its lines, graded with what it wrote;
// another agent's aren't.
func TestWordsClockKeepsSetupsNotes(t *testing.T) {
	w := &wordsClock{Model: deltas{"(note) Miro la cuenta…"}, setup: "You are Coach."}
	for _, system := range []string{"You are Coach.\n\nskills…", "You are the Refunds taf."} {
		if _, err := w.Turn(context.Background(), model.Request{System: system}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := w.setupNotes(); !slices.Equal(got, []string{"Miro la cuenta…"}) {
		t.Fatalf("notes %q", got)
	}
}

// Each message's wait for Setup's first words splits into the run's start,
// whole turns before the words' turn, the tools between them, and in the
// words' turn its first content, then the words: the parts add up to the
// wait (kiff-cloud#1487's latency hunt). A message with no words has none.
func TestAWaitSplitsWhereItWent(t *testing.T) {
	t0 := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	turns := []clockTurn{
		// Message 1: a turn with no words that shows a card, then the words.
		{began: at(500), ended: at(4000), shown: at(4000), first: 1500 * time.Millisecond, prompt: 21000, cached: 20000, output: 300, written: 400},
		{began: at(5000), ended: at(9000), words: at(8000), first: 2000 * time.Millisecond, prompt: 22000, cached: 21500, output: 200, written: 200},
		// Message 2: the words in its first turn.
		{began: at(20400), ended: at(24000), words: at(23000), first: 1600 * time.Millisecond, prompt: 31000, cached: 21000, output: 120, written: 80},
		// Message 3: a turn, and no words before the end.
		{began: at(40300), ended: at(41000), first: 700 * time.Millisecond, output: 10},
	}
	sp := splitWaits([]time.Time{t0, at(20000), at(40000)}, turns, at(60000))
	if len(sp) != 3 {
		t.Fatalf("%d splits", len(sp))
	}
	one := sp[0]
	if one.wait != 8*time.Second || one.start != 500*time.Millisecond || one.before != 1 || one.turns != 3500*time.Millisecond ||
		one.between != time.Second || one.first != 2*time.Second || one.toWords != time.Second || one.thinking != (300-100)+(200-50) ||
		one.prompt != 21000 || one.cached != 20000 {
		t.Fatalf("message 1: %+v", one)
	}
	if one.start+one.turns+one.between+one.first+one.toWords != one.wait {
		t.Fatalf("message 1's parts don't add up: %+v", one)
	}
	// The owner saw its card at 4 s, before the words at 8 s.
	if one.seen != 4*time.Second {
		t.Fatalf("message 1 first seen after %s, want its card's 4s", one.seen)
	}
	two := sp[1]
	if two.wait != 3*time.Second || two.start != 400*time.Millisecond || two.before != 0 || two.turns != 0 || two.first != 1600*time.Millisecond || two.toWords != time.Second {
		t.Fatalf("message 2: %+v", two)
	}
	if sp[2].wait != -1 || sp[2].seen != -1 {
		t.Fatalf("message 3 had nothing seen: %+v", sp[2])
	}
	if two.seen != two.wait {
		t.Fatalf("message 2's words were the first thing seen: %+v", two)
	}
	if got := two.line(); got != "first seen after 3s; first words after 3s: start 400ms, 0 turns before 0s, tools 0s, first token 1.6s, then the words 1s; thinking about 100 tokens; prompt 31000 tokens, 67% from the cache" {
		t.Fatalf("line %q", got)
	}
	med, n := medianSplit(sp)
	if n != 2 || med.wait != 8*time.Second || med.first != 2*time.Second {
		t.Fatalf("medians over %d: %+v", n, med)
	}
}

// The clock keeps Setup's turns, with their times and usage, and no other
// agent's.
func TestWordsClockKeepsSetupsTurns(t *testing.T) {
	w := &wordsClock{Model: usageOf{deltas{"(note) Miro la cuenta…"}, model.Usage{InputTokens: 10, CacheRead: 900, CacheWrite: 90, OutputTokens: 40}}, setup: "You are Coach."}
	for _, system := range []string{"You are Coach.\n\nskills…", "You are the Refunds taf."} {
		if _, err := w.Turn(context.Background(), model.Request{System: system}, nil); err != nil {
			t.Fatal(err)
		}
	}
	turns := w.setupTurns()
	if len(turns) != 1 || turns[0].words.IsZero() || turns[0].ended.Before(turns[0].words) || turns[0].prompt != 1000 || turns[0].cached != 900 || turns[0].output != 40 {
		t.Fatalf("turns %+v", turns)
	}
}

// usageOf is a model that reports usage for each turn.
type usageOf struct {
	model.Model
	usage model.Usage
}

func (u usageOf) Turn(ctx context.Context, req model.Request, onText func(string)) (model.Reply, error) {
	r, err := u.Model.Turn(ctx, req, onText)
	r.Usage = u.usage
	return r, err
}

// The clock says where a turn's prompt stopped matching the turn before's,
// short of its end: the system prompt, the tools, or a message with its
// kinds and sizes, never a word; and nothing when the prompt only grew.
func TestTheClockSaysWhereAPromptBroke(t *testing.T) {
	user := func(text string) model.Message {
		return model.Message{Role: model.User, Blocks: []model.Block{{Kind: model.Text, Text: text}}}
	}
	base := model.Request{System: "You are Coach.", Tools: []model.ToolSpec{{Name: "show"}}, Messages: []model.Message{user("Set up refunds"), user("[called] list → done")}}
	grew := base
	grew.Messages = append(slices.Clone(base.Messages), user("Yes"))
	if got := prefixBroke(&base, grew); got != "" {
		t.Fatalf("a prompt that only grew: %q", got)
	}
	if got := prefixBroke(nil, base); got != "" {
		t.Fatalf("the first turn: %q", got)
	}
	changed := grew
	changed.Messages = slices.Clone(grew.Messages)
	changed.Messages[1] = user("[called] list → done; the tool answered…")
	if got := prefixBroke(&base, changed); got != "message 2 of 2 (user, block 1: text 22 bytes → text 44 bytes)" {
		t.Fatalf("a changed message: %q", got)
	}
	if strings.Contains(prefixBroke(&base, changed), "called") {
		t.Fatal("a word of the chat in the line")
	}
	skills := base
	skills.System += " Skill: halting"
	if got := prefixBroke(&base, skills); got != "system (14 → 29 bytes)" {
		t.Fatalf("the system prompt: %q", got)
	}
	deferred := base
	deferred.Tools = []model.ToolSpec{{Name: "show", Defer: true}}
	if got := prefixBroke(&base, deferred); got != "tools (1 → 1)" {
		t.Fatalf("the tools: %q", got)
	}
	short := base
	short.Messages = base.Messages[:1]
	if got := prefixBroke(&base, short); got != "message 2 of 2 (gone)" {
		t.Fatalf("a message gone: %q", got)
	}
}
