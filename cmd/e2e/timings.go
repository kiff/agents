package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/session"
)

// setupTimings writes each of the setup chat's model turns to the
// transcript, never the log: its speed, how long it took, how long before
// its first content, the cache it read and wrote and what it cost; then
// the chat's totals. They are what kiff/agents#197 is judged by, before and after.
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
		t.logf("setup turn %d: speed %s, %d ms, first %d ms, output %d tokens (thinking about %d), thinking summary %d B, cache read %d, written %d, cost %s",
			i+1, u.speed, u.ms, u.first, u.output, u.thinking(), u.thinkingBytes, u.read, u.written, usd(u.cost))
	}
	s := summarize(turns)
	if s.turns > 0 {
		t.logf("setup turns: %d (%d fast), %s in all, first content at %s median, cache read %d, written %d, cost %s",
			s.turns, s.fast, s.total, s.medianFirst, s.read, s.written, usd(s.cost))
	}
	// What the owner waited for: from each message they sent in the chat to
	// Coach's first words after it. The summary goes to the log too: it's
	// times and a cost, nothing of the chat.
	if t.words == nil {
		return
	}
	waits := firstWords(t.sent, t.words.times(), time.Now())
	for i, w := range waits {
		if w < 0 {
			t.logf("setup message %d: no words before the next message", i+1)
			continue
		}
		t.logf("setup message %d: first words after %s", i+1, w.Round(10*time.Millisecond))
	}
	if med, p90, n := percentiles(waits); n > 0 {
		line := fmt.Sprintf("setup speed: first words median %s, p90 %s, over %d messages; %d turns, output %d tokens (thinking about %d); cost %s",
			med.Round(10*time.Millisecond), p90.Round(10*time.Millisecond), n, s.turns, s.output, s.thinking, usd(s.cost))
		t.logf("%s", line)
		fmt.Fprintln(t.cfg.log, line)
	}
}

// wordsClock is the model, timing the first words of each of its turns that
// a person waiting sees: its first text, or its first progress note between
// tool calls (thinking "updates"), whichever comes first. Every turn goes
// through it; what's read is only when.
type wordsClock struct {
	model.Model
	mu    sync.Mutex
	texts []time.Time
	// setup is Setup's instructions, which its turns' system prompt starts
	// with, and notes its progress notes: lines, graded with what it wrote.
	setup string
	notes []string
}

// setupNotes is every progress note Setup wrote, in order.
func (w *wordsClock) setupNotes() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.notes)
}

func (w *wordsClock) Turn(ctx context.Context, req model.Request, onText func(string)) (model.Reply, error) {
	first := true
	words := func(s string) {
		if first && strings.TrimSpace(s) != "" {
			first = false
			w.mu.Lock()
			w.texts = append(w.texts, time.Now())
			w.mu.Unlock()
		}
	}
	onUpdate := req.OnUpdate
	setup := w.setup != "" && strings.HasPrefix(req.System, w.setup)
	req.OnUpdate = func(note string) {
		words(note)
		if setup {
			w.mu.Lock()
			w.notes = append(w.notes, note)
			w.mu.Unlock()
		}
		if onUpdate != nil {
			onUpdate(note)
		}
	}
	return w.Model.Turn(ctx, req, func(text string) {
		words(text)
		if onText != nil {
			onText(text)
		}
	})
}

// times is when each turn that wrote words wrote its first, in order.
func (w *wordsClock) times() []time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.texts)
}

// firstWords is, for each message sent before end, how long until the first
// text after it and before the next message (or end): -1 when none came.
func firstWords(sent, texts []time.Time, end time.Time) []time.Duration {
	var out []time.Duration
	for i, at := range sent {
		if !at.Before(end) {
			break
		}
		until := end
		if i+1 < len(sent) && sent[i+1].Before(end) {
			until = sent[i+1]
		}
		wait := time.Duration(-1)
		for _, tx := range texts {
			if !tx.Before(at) && tx.Before(until) {
				wait = tx.Sub(at)
				break
			}
		}
		out = append(out, wait)
	}
	return out
}

// percentiles is the median and the 90th percentile of the waits that came,
// and how many came.
func percentiles(waits []time.Duration) (median, p90 time.Duration, n int) {
	var got []time.Duration
	for _, w := range waits {
		if w >= 0 {
			got = append(got, w)
		}
	}
	if len(got) == 0 {
		return 0, 0, 0
	}
	slices.Sort(got)
	at := func(q float64) time.Duration {
		i := int(q*float64(len(got))+0.999999) - 1
		return got[max(0, min(i, len(got)-1))]
	}
	return got[len(got)/2], at(0.9), len(got)
}

type turnTiming struct {
	speed         string
	ms, first     int64
	read, written int64
	cost          int64 // millionths of a US dollar, as the turn's usage records it
	output        int64 // output tokens, thinking included
	thinkingBytes int   // the thinking's summary, not the thinking itself
	writtenBytes  int   // text and tool input the turn wrote
}

// thinking estimates the turn's thinking tokens: its output tokens less what
// it wrote that shows (text and tool input, about 4 bytes a token). The
// usage records no thinking tokens, and its thinking bytes are the summary.
func (u turnTiming) thinking() int64 {
	return max(0, u.output-int64(u.writtenBytes)/4)
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
		tt := turnTiming{speed: u.Speed, read: u.CacheRead, written: u.CacheWrite + u.CacheWrite1h, cost: u.CostMicroUSD, output: u.OutputTokens}
		if tt.speed == "" {
			tt.speed = "standard"
		}
		if u.Timing != nil {
			tt.ms, tt.first = u.Timing.MS, u.Timing.FirstMS
			tt.thinkingBytes, tt.writtenBytes = u.Timing.ThinkingBytes, u.Timing.TextBytes+u.Timing.ToolInputBytes
		}
		out = append(out, tt)
	}
	return out
}

type turnSummary struct {
	turns, fast      int
	total            time.Duration
	medianFirst      time.Duration
	read, written    int64
	cost             int64
	output, thinking int64
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
		s.cost += u.cost
		s.output += u.output
		s.thinking += u.thinking()
		firsts = append(firsts, u.first)
	}
	if len(firsts) > 0 {
		slices.Sort(firsts)
		s.medianFirst = time.Duration(firsts[len(firsts)/2]) * time.Millisecond
	}
	return s
}

// usd is a cost in millionths of a US dollar, to the cent's tenth.
func usd(micro int64) string { return fmt.Sprintf("$%.3f", float64(micro)/1e6) }
