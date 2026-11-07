package model

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// Sourced runs each turn where its run belongs: on Primary (the Claude API,
// with KIFF's key) or on Backup (Claude on Amazon Bedrock, in our own AWS
// account). Both are off by default: Source is SourceAnthropic and Fallback
// false unless configured.
//
//   - Source says where a new run goes.
//   - With Fallback, a new run whose first turn Primary refuses for want of
//     credits, or can't serve once its own retries are spent (FallsBack),
//     goes to Backup instead, and stays there. Only before anything of the
//     turn streamed: what someone saw is never said twice.
//   - A run under way never moves (Request.Source): one that started on
//     Primary fails as it would without a backup.
//   - Backup's turns count against a day's cap (Cap). Without one it
//     refuses, unless Exempt (the Eval, which has its own budget).
type Sourced struct {
	Primary, Backup Model
	Source          string
	Fallback        bool
	// FallsBack says whether Primary's error lets a new run go to Backup.
	FallsBack func(error) bool
	Cap       *DailyCap
	Exempt    bool
	Now       func() time.Time
}

// DailyCap bounds what Backup's turns may cost in a UTC day, in millionths
// of a US dollar, counted in Spend under a key of its own. Each turn
// reserves its worst case before it runs (WorstCase, at PriceFactor, the
// backup's price over the list's; 0 is 1), and settles to what it cost
// after: turns that start together can't pass the cap between them.
type DailyCap struct {
	MicroUSD    int64
	Spend       DaySpend
	PriceFactor float64
}

// DaySpend keeps a day's total. Reserve adds only while the total stays
// within a limit; both are atomic, and Add takes a negative amount back.
type DaySpend interface {
	Reserve(ctx context.Context, day string, microUSD, limit int64) (bool, error)
	Add(ctx context.Context, day string, microUSD int64) error
}

// MaxOutput is the output a turn is reserved for when its request names no
// MaxTokens: the most a model writes in one turn.
const MaxOutput = 128_000

// WorstCase is the most req's turn can cost at p, times factor, in
// millionths of a US dollar: every byte the model reads (the system prompt,
// every tool, deferred ones included, and the messages) counted as a token
// at the dearest of input and cache writes, plus all of MaxTokens as output.
// A token is never shorter than a byte, so the turn's real cost is below it.
func WorstCase(req Request, p Price, factor float64) int64 {
	in := int64(len(req.System))
	for _, t := range req.Tools {
		in += int64(len(t.Name) + len(t.Description) + len(t.InputSchema))
	}
	in += int64(req.Size().Messages)
	out := int64(req.MaxTokens)
	if out <= 0 {
		out = MaxOutput
	}
	worst := p.Cost(Usage{InputTokens: in, OutputTokens: out})
	if dearer := max(p.CacheWrite, p.CacheWrite1h); dearer > p.Input {
		worst = (in*dearer + out*p.Output + 999_999) / 1_000_000
	}
	if factor > 1 {
		worst = int64(math.Ceil(float64(worst) * factor))
	}
	return max(worst, 1) // a turn always needs room under the cap
}

// ErrBackupOff is a turn Backup refused because it has no daily cap.
var ErrBackupOff = errors.New("model: the Bedrock backup is off: no daily cap is set")

// ErrBackupCapped is a turn Backup refused because the day's cap is spent.
var ErrBackupCapped = errors.New("model: the Bedrock backup has spent today's cap")

func (s *Sourced) Turn(ctx context.Context, req Request, onText func(string)) (Reply, error) {
	switch {
	case req.Source == SourceBedrock, req.Source == "" && s.Source == SourceBedrock:
		return s.backup(ctx, req, onText)
	case req.Source != "": // under way on Primary: it stays
		return s.primary(ctx, req, onText)
	}
	streamed := false
	seen := func(f func(string)) func(string) {
		return func(t string) {
			streamed = true
			if f != nil {
				f(t)
			}
		}
	}
	first := req
	first.OnThinking, first.OnUpdate = seen(req.OnThinking), seen(req.OnUpdate)
	r, err := s.primary(ctx, first, seen(onText))
	if err == nil || !s.Fallback || s.Backup == nil || streamed || s.FallsBack == nil || !s.FallsBack(err) {
		return r, err
	}
	b, berr := s.backup(ctx, req, onText)
	if berr != nil {
		return b, errors.Join(err, berr)
	}
	return b, nil
}

func (s *Sourced) primary(ctx context.Context, req Request, onText func(string)) (Reply, error) {
	r, err := s.Primary.Turn(ctx, req, onText)
	if r.Source == "" {
		r.Source = SourceAnthropic
	}
	return r, err
}

func (s *Sourced) backup(ctx context.Context, req Request, onText func(string)) (Reply, error) {
	if s.Backup == nil {
		return Reply{}, ErrBackupOff
	}
	day := s.now().UTC().Format(time.DateOnly)
	var reserved int64
	capped := !s.Exempt
	if capped {
		if s.Cap == nil || s.Cap.MicroUSD <= 0 || s.Cap.Spend == nil {
			return Reply{}, ErrBackupOff
		}
		reserved = WorstCase(req, req.Price, s.Cap.PriceFactor)
		ok, err := s.Cap.Spend.Reserve(ctx, day, reserved, s.Cap.MicroUSD)
		if err != nil {
			return Reply{}, fmt.Errorf("model: reserving the Bedrock backup's turn: %w", err)
		}
		if !ok {
			return Reply{}, ErrBackupCapped
		}
	}
	r, err := s.Backup.Turn(ctx, req, onText)
	if r.Source == "" {
		r.Source = SourceBedrock
	}
	if capped {
		// Settled to what it cost; a turn that failed gives its reservation
		// back. A settle that fails leaves the worst case counted, the safe
		// side, unless the turn cost more than it, which WorstCase rules out.
		cost := r.Cost(req.Price)
		if serr := s.Cap.Spend.Add(ctx, day, cost-reserved); serr != nil && cost > reserved {
			err = errors.Join(err, fmt.Errorf("model: counting the Bedrock backup's spend: %w", serr))
		}
	}
	return r, err
}

func (s *Sourced) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Cost is what the reply's usage costs at p, times its PriceFactor, rounded
// up: what the turn costs where it ran.
func (r Reply) Cost(p Price) int64 {
	c := p.Cost(r.Usage)
	if r.PriceFactor > 0 && r.PriceFactor != 1 {
		c = int64(math.Ceil(float64(c) * r.PriceFactor))
	}
	return c
}

// Window is the context window from wherever knows it: Primary first, then
// Backup (a model without the Models API knows its own).
func (s *Sourced) Window(ctx context.Context, id string) (int64, error) {
	var errs []error
	for _, m := range []Model{s.Primary, s.Backup} {
		if w, ok := m.(Windower); ok && m != nil {
			n, err := w.Window(ctx, id)
			if err == nil {
				return n, nil
			}
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return 0, errors.New("model: no source knows the context window")
	}
	return 0, errors.Join(errs...)
}
