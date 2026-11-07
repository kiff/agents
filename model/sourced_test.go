package model

import (
	"context"
	"errors"
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

func (d days) Spent(_ context.Context, day string) (int64, error) { return d[day], nil }
func (d days) Add(_ context.Context, day string, micro int64) error {
	d[day] += micro
	return nil
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
