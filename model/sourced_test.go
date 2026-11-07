package model

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type scripted struct {
	name    string
	err     error
	stream  bool
	calls   int
	factor  float64
	lastReq Request
}

func (s *scripted) Turn(_ context.Context, req Request, onText func(string)) (Reply, error) {
	s.calls++
	s.lastReq = req
	if s.stream && onText != nil {
		onText("partly ")
	}
	if s.err != nil {
		return Reply{}, s.err
	}
	return Reply{Usage: Usage{InputTokens: 1_000_000}, PriceFactor: s.factor}, nil
}

type days map[string]int64

func (d days) Reserve(_ context.Context, day string, micro, limit int64) (bool, error) {
	if d[day]+micro > limit {
		return false, nil
	}
	d[day] += micro
	return true, nil
}

func (d days) Add(_ context.Context, day string, micro int64) error {
	d[day] += micro
	return nil
}

// lockedDays is days for turns at once.
type lockedDays struct {
	mu sync.Mutex
	d  days
}

func (l *lockedDays) Reserve(ctx context.Context, day string, micro, limit int64) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.d.Reserve(ctx, day, micro, limit)
}

func (l *lockedDays) Add(ctx context.Context, day string, micro int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.d.Add(ctx, day, micro)
}

var errCredits = errors.New("400 invalid_request_error: Your credit balance is too low")

func fallsBack(err error) bool { return errors.Is(err, errCredits) }

func sourced(primaryErr error, spend days, cap int64) (*Sourced, *scripted, *scripted) {
	p := &scripted{name: "anthropic", err: primaryErr}
	b := &scripted{name: "bedrock", factor: 1.1}
	s := &Sourced{Primary: p, Backup: b, Fallback: true, FallsBack: fallsBack,
		Cap: &DailyCap{MicroUSD: cap, Spend: spend}, Now: func() time.Time { return time.Date(2026, 11, 7, 13, 0, 0, 0, time.UTC) }}
	return s, p, b
}

var price = Price{Input: 2_000_000}

// A new run's first turn, refused for credits, runs on the backup and says
// so; its cost, at the backup's price, counts against the day's cap.
func TestANewRunFallsBackOnTheCreditRefusal(t *testing.T) {
	spend := days{}
	s, p, b := sourced(errCredits, spend, 5_000_000)
	r, err := s.Turn(context.Background(), Request{Price: price}, nil)
	if err != nil || p.calls != 1 || b.calls != 1 || r.Source != SourceBedrock {
		t.Fatalf("reply %+v, err %v, calls %d/%d", r, err, p.calls, b.calls)
	}
	if spend["2026-11-07"] != 2_200_000 { // $2 of input at 1.1
		t.Fatalf("spend %v", spend)
	}
	// Without the error, the run stays on the primary.
	s, p, b = sourced(nil, days{}, 5_000_000)
	if r, err := s.Turn(context.Background(), Request{}, nil); err != nil || r.Source != SourceAnthropic || b.calls != 0 {
		t.Fatalf("no error: reply %+v, err %v, backup calls %d", r, err, b.calls)
	}
}

// Never: a run under way, a turn that already streamed, an error that
// doesn't fall back, or with the fallback off.
func TestOnlyANewRunsFirstTurnFallsBack(t *testing.T) {
	other := errors.New("400 messages: roles must alternate")
	for _, c := range []struct {
		name string
		req  Request
		err  error
		set  func(*Sourced, *scripted)
	}{
		{"a run under way on the primary", Request{Source: SourceAnthropic}, errCredits, nil},
		{"another error", Request{}, other, nil},
		{"part of the turn streamed", Request{}, errCredits, func(_ *Sourced, p *scripted) { p.stream = true }},
		{"the fallback off", Request{}, errCredits, func(s *Sourced, _ *scripted) { s.Fallback = false }},
	} {
		s, p, b := sourced(c.err, days{}, 5_000_000)
		if c.set != nil {
			c.set(s, p)
		}
		if _, err := s.Turn(context.Background(), c.req, func(string) {}); !errors.Is(err, c.err) || b.calls != 0 {
			t.Errorf("%s: err %v, backup calls %d", c.name, err, b.calls)
		}
	}
	// A run that started on the backup stays there.
	s, p, b := sourced(nil, days{}, 5_000_000)
	if r, _ := s.Turn(context.Background(), Request{Source: SourceBedrock}, nil); r.Source != SourceBedrock || p.calls != 0 || b.calls != 1 {
		t.Fatalf("pinned to the backup: %+v, calls %d/%d", r, p.calls, b.calls)
	}
	// With the source set to the backup, a new run starts there.
	s, p, b = sourced(nil, days{}, 5_000_000)
	s.Source = SourceBedrock
	if r, _ := s.Turn(context.Background(), Request{}, nil); r.Source != SourceBedrock || p.calls != 0 {
		t.Fatalf("source bedrock: %+v, primary calls %d", r, p.calls)
	}
}

// The backup refuses without a cap, and past the day's cap, in its own words,
// with the primary's error kept; the Eval is exempt.
func TestTheBackupKeepsToItsDailyCap(t *testing.T) {
	s, _, b := sourced(errCredits, days{}, 0)
	s.Cap = nil
	if _, err := s.Turn(context.Background(), Request{}, nil); !errors.Is(err, ErrBackupOff) || !errors.Is(err, errCredits) || b.calls != 0 {
		t.Fatalf("no cap: %v, backup calls %d", err, b.calls)
	}
	spend := days{"2026-11-07": 5_000_000}
	s, _, b = sourced(errCredits, spend, 5_000_000)
	if _, err := s.Turn(context.Background(), Request{}, nil); !errors.Is(err, ErrBackupCapped) || b.calls != 0 {
		t.Fatalf("capped: %v, backup calls %d", err, b.calls)
	}
	s.Exempt = true
	if r, err := s.Turn(context.Background(), Request{}, nil); err != nil || r.Source != SourceBedrock {
		t.Fatalf("exempt: %+v, %v", r, err)
	}
	if spend["2026-11-07"] != 5_000_000 {
		t.Fatalf("an exempt turn counted: %v", spend)
	}
}

func TestACostAtTheBackupsPrice(t *testing.T) {
	r := Reply{Usage: Usage{InputTokens: 1_000_000}, PriceFactor: 1.1}
	if got := r.Cost(Price{Input: 2_000_000}); got != 2_200_000 {
		t.Fatalf("cost %d", got)
	}
	if got := (Reply{Usage: Usage{InputTokens: 1_000_000}}).Cost(Price{Input: 2_000_000}); got != 2_000_000 {
		t.Fatalf("no factor: %d", got)
	}
}

// A turn's worst case: every byte read as a token at the dearest input price,
// deferred tools included, plus all its output, at the backup's factor.
func TestATurnsWorstCase(t *testing.T) {
	p := Price{Input: 3_000_000, Output: 15_000_000, CacheWrite: 3_750_000, CacheWrite1h: 6_000_000}
	req := Request{System: strings.Repeat("s", 1_000), MaxTokens: 2_000,
		Tools:    []ToolSpec{{Name: "a", Description: strings.Repeat("d", 499), InputSchema: []byte("{}")}, {Name: "b", Defer: true, InputSchema: []byte(strings.Repeat("x", 500))}},
		Messages: []Message{{Role: User, Blocks: []Block{{Kind: Text, Text: strings.Repeat("m", 3_000)}}}}}
	// 1,000 + 502 + 501 + 3,000 bytes = 5,003 tokens at the hour's cache write ($6/M), and 2,000 out at $15/M.
	in := int64(1_000 + 1 + 499 + 2 + 1 + 500 + 3_000)
	want := (in*6_000_000 + 2_000*15_000_000 + 999_999) / 1_000_000
	if got := WorstCase(req, p, 1); got != want {
		t.Fatalf("worst case %d, want %d", got, want)
	}
	if got := WorstCase(req, p, 1.1); got != int64(math.Ceil(float64(want)*1.1)) {
		t.Fatalf("at 1.1: %d", got)
	}
	// No MaxTokens: a whole turn's output; no prices: still a credit's room.
	if got := WorstCase(Request{}, Price{Output: 1_000_000}, 1); got != MaxOutput {
		t.Fatalf("no MaxTokens: %d, want %d", got, MaxOutput)
	}
	if got := WorstCase(Request{}, Price{}, 1); got != 1 {
		t.Fatalf("unpriced: %d", got)
	}
}

// A turn that fails gives its reservation back; one that runs is counted at
// what it cost, not at its worst case.
func TestATurnSettlesToWhatItCost(t *testing.T) {
	spend := days{}
	s, _, b := sourced(errCredits, spend, 50_000_000)
	b.err = errors.New("503 bedrock unavailable")
	req := Request{Price: Price{Input: 2_000_000, Output: 10_000_000}, MaxTokens: 1_000}
	if _, err := s.Turn(context.Background(), req, nil); err == nil || spend["2026-11-07"] != 0 {
		t.Fatalf("a failed turn: %v, spend %v", err, spend)
	}
	b.err = nil
	if _, err := s.Turn(context.Background(), req, nil); err != nil || spend["2026-11-07"] != 2_200_000 {
		t.Fatalf("a turn: %v, spend %v, want its cost, not its worst case", err, spend)
	}
}

// Turns that start together can't pass the cap between them: each reserves
// its worst case first, and those that don't fit are refused before running.
func TestTurnsAtOnceKeepToTheCap(t *testing.T) {
	req := Request{Price: Price{Input: 2_000_000, Output: 10_000_000}, MaxTokens: 1_000}
	worst := WorstCase(req, req.Price, 1.1)
	spend := &lockedDays{d: days{}}
	start := make(chan struct{})
	b := &blocking{start: start}
	s := &Sourced{Primary: &scripted{err: errCredits}, Backup: b, Fallback: true, FallsBack: fallsBack,
		Cap: &DailyCap{MicroUSD: 3*worst + 1, Spend: spend, PriceFactor: 1.1}, Now: func() time.Time { return time.Date(2026, 11, 7, 13, 0, 0, 0, time.UTC) }}
	const n = 20
	errs := make(chan error, n)
	for range n {
		go func() { _, err := s.Turn(context.Background(), req, nil); errs <- err }()
	}
	refused := 0
	for range n - 3 { // the refused don't wait for the turns that run
		if err := <-errs; !errors.Is(err, ErrBackupCapped) {
			t.Fatalf("a turn past the cap: %v", err)
		}
		refused++
	}
	close(start)
	for range 3 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if b.calls.Load() != 3 || refused != n-3 || spend.d["2026-11-07"] != 3*2_200_000 {
		t.Fatalf("%d ran, %d refused, spend %v", b.calls.Load(), refused, spend.d)
	}
}

// blocking is a backup whose turns wait for start, each costing $2 of input.
type blocking struct {
	start chan struct{}
	calls atomic.Int32
}

func (b *blocking) Turn(context.Context, Request, func(string)) (Reply, error) {
	b.calls.Add(1)
	<-b.start
	return Reply{Usage: Usage{InputTokens: 1_000_000}, PriceFactor: 1.1}, nil
}
