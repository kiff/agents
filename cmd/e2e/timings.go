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
	// Where each run's start went, step by step (session.TurnTiming.Start):
	// times only, so the log too.
	for i, st := range runStarts(events) {
		fmt.Fprintf(t.cfg.log, "setup run %d start: %s\n", i+1, st)
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
	now := time.Now()
	waits := firstWords(t.sent, t.words.times(), now)
	splits := splitWaits(t.sent, t.words.setupTurns(), now)
	for i, w := range waits {
		if w < 0 {
			t.logf("setup message %d: no words before the next message", i+1)
			continue
		}
		t.logf("setup message %d: first words after %s", i+1, w.Round(10*time.Millisecond))
		// Where the wait went, in times and counts: the log too (kiff-cloud#1487's latency hunt).
		if i < len(splits) && splits[i].wait >= 0 {
			fmt.Fprintf(t.cfg.log, "setup message %d: %s\n", i+1, splits[i].line())
		}
	}
	if med, n := medianSplit(splits); n > 0 {
		fmt.Fprintf(t.cfg.log, "setup words split, medians over %d messages: %s\n", n, med.line())
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
	// turns are Setup's turns as the clock saw them, for where each wait
	// for its first words went (splitWaits).
	turns []clockTurn
}

// clockTurn is one of Setup's model turns as the clock saw it: when it began
// and ended, when its first words came (zero: none), and what the provider
// said of it. Times and counts only, never a word.
type clockTurn struct {
	began, ended, words time.Time
	first               time.Duration // to its first streamed content
	prompt, cached      int64         // its prompt's tokens, and those read from the cache
	output              int64         // output tokens, thinking included
	written             int           // bytes of text and tool input it wrote
}

// thinking estimates the turn's thinking tokens, as turnTiming.thinking does.
func (c clockTurn) thinking() int64 { return max(0, c.output-int64(c.written)/4) }

// setupTurns is Setup's turns so far, in order.
func (w *wordsClock) setupTurns() []clockTurn {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.turns)
}

// setupNotes is every progress note Setup wrote, in order.
func (w *wordsClock) setupNotes() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.notes)
}

func (w *wordsClock) Turn(ctx context.Context, req model.Request, onText func(string)) (model.Reply, error) {
	began := time.Now()
	var wordsAt time.Time
	words := func(s string) {
		if wordsAt.IsZero() && strings.TrimSpace(s) != "" {
			wordsAt = time.Now()
			w.mu.Lock()
			w.texts = append(w.texts, wordsAt)
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
	reply, err := w.Model.Turn(ctx, req, func(text string) {
		words(text)
		if onText != nil {
			onText(text)
		}
	})
	if setup && err == nil {
		u := reply.Usage
		c := clockTurn{began: began, ended: time.Now(), words: wordsAt, first: reply.FirstToken,
			prompt: u.InputTokens + u.CacheRead + u.CacheWrite + u.CacheWrite1h, cached: u.CacheRead, output: u.OutputTokens}
		for _, b := range reply.Blocks {
			c.written += len(b.Text) + len(b.Input)
		}
		w.mu.Lock()
		w.turns = append(w.turns, c)
		w.mu.Unlock()
	}
	return reply, err
}

// wordsSplit is where one message's wait for Setup's first words went:
// from the message to its run's first turn (the run's start), whole turns
// before the one with the words, the tool calls and runtime between them,
// and in the words' turn, its first streamed content, then the words. Times
// and counts only.
type wordsSplit struct {
	wait     time.Duration // to the first words; -1: none came
	start    time.Duration // to the first model turn
	before   int           // whole turns before the words' turn
	turns    time.Duration // those turns' time
	between  time.Duration // tool calls and the runtime between turns, before the words' turn
	first    time.Duration // the words' turn: to its first streamed content
	toWords  time.Duration // the words' turn: from its first content to the words
	thinking int64         // thinking tokens, estimated, up to and with the words' turn
	prompt   int64         // the first turn's prompt, in tokens
	cached   int64         // what of it came from the cache
}

// splitWaits is, for each message sent before end, where its wait for the
// first words went, from Setup's turns: the turns that began after it and
// before the next message (or end).
func splitWaits(sent []time.Time, turns []clockTurn, end time.Time) []wordsSplit {
	var out []wordsSplit
	for i, at := range sent {
		if !at.Before(end) {
			break
		}
		until := end
		if i+1 < len(sent) && sent[i+1].Before(end) {
			until = sent[i+1]
		}
		var in []clockTurn
		for _, c := range turns {
			if !c.began.Before(at) && c.began.Before(until) {
				in = append(in, c)
			}
		}
		sp := wordsSplit{wait: -1}
		k := slices.IndexFunc(in, func(c clockTurn) bool { return !c.words.IsZero() && c.words.Before(until) })
		if k < 0 {
			out = append(out, sp)
			continue
		}
		w := in[k]
		sp.wait, sp.start, sp.before = w.words.Sub(at), in[0].began.Sub(at), k
		sp.prompt, sp.cached = in[0].prompt, in[0].cached
		for j, c := range in[:k+1] {
			sp.thinking += c.thinking()
			if j < k {
				sp.turns += c.ended.Sub(c.began)
				sp.between += in[j+1].began.Sub(c.ended)
			}
		}
		sp.first = w.first
		sp.toWords = max(0, w.words.Sub(w.began)-w.first)
		out = append(out, sp)
	}
	return out
}

// line is the split on one line, rounded to tens of milliseconds.
func (sp wordsSplit) line() string {
	r := func(d time.Duration) time.Duration { return d.Round(10 * time.Millisecond) }
	share := 0
	if sp.prompt > 0 {
		share = int(100 * sp.cached / sp.prompt)
	}
	return fmt.Sprintf("first words after %s: start %s, %d turns before %s, tools %s, first token %s, then the words %s; thinking about %d tokens; prompt %d tokens, %d%% from the cache",
		r(sp.wait), r(sp.start), sp.before, r(sp.turns), r(sp.between), r(sp.first), r(sp.toWords), sp.thinking, sp.prompt, share)
}

// medianSplit is the median of each part of the splits that came, and how
// many came.
func medianSplit(splits []wordsSplit) (wordsSplit, int) {
	var got []wordsSplit
	for _, sp := range splits {
		if sp.wait >= 0 {
			got = append(got, sp)
		}
	}
	if len(got) == 0 {
		return wordsSplit{wait: -1}, 0
	}
	med := func(f func(wordsSplit) int64) int64 {
		v := make([]int64, len(got))
		for i, sp := range got {
			v[i] = f(sp)
		}
		slices.Sort(v)
		return v[len(v)/2]
	}
	d := func(f func(wordsSplit) time.Duration) time.Duration {
		return time.Duration(med(func(sp wordsSplit) int64 { return int64(f(sp)) }))
	}
	return wordsSplit{
		wait:     d(func(sp wordsSplit) time.Duration { return sp.wait }),
		start:    d(func(sp wordsSplit) time.Duration { return sp.start }),
		before:   int(med(func(sp wordsSplit) int64 { return int64(sp.before) })),
		turns:    d(func(sp wordsSplit) time.Duration { return sp.turns }),
		between:  d(func(sp wordsSplit) time.Duration { return sp.between }),
		first:    d(func(sp wordsSplit) time.Duration { return sp.first }),
		toWords:  d(func(sp wordsSplit) time.Duration { return sp.toWords }),
		thinking: med(func(sp wordsSplit) int64 { return sp.thinking }),
		prompt:   med(func(sp wordsSplit) int64 { return sp.prompt }),
		cached:   med(func(sp wordsSplit) int64 { return sp.cached }),
	}, len(got)
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

// runStarts is each run's start in events, in order, as its first turn
// records it: the whole, then each step's milliseconds.
func runStarts(events []session.Event) []string {
	var out []string
	for _, e := range events {
		var u session.UsageBody
		if e.Kind != session.Usage || json.Unmarshal(e.Body, &u) != nil || u.Timing == nil || u.Timing.StartMS == 0 {
			continue
		}
		parts := make([]string, len(u.Timing.Start))
		for i, sp := range u.Timing.Start {
			parts[i] = fmt.Sprintf("%s %d", sp.Name, sp.MS)
		}
		line := fmt.Sprintf("%d ms", u.Timing.StartMS)
		if len(parts) > 0 {
			line += ": " + strings.Join(parts, ", ")
		}
		out = append(out, line)
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
