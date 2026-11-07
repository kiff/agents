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
// of a US dollar, counted in Spend under a key of its own.
type DailyCap struct {
	MicroUSD int64
	Spend    DaySpend
}

// DaySpend keeps a day's total. Add is atomic.
type DaySpend interface {
	Spent(ctx context.Context, day string) (int64, error)
	Add(ctx context.Context, day string, microUSD int64) error
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
	if !s.Exempt {
		if s.Cap == nil || s.Cap.MicroUSD <= 0 || s.Cap.Spend == nil {
			return Reply{}, ErrBackupOff
		}
		spent, err := s.Cap.Spend.Spent(ctx, day)
		if err != nil {
			return Reply{}, fmt.Errorf("model: reading the Bedrock backup's spend today: %w", err)
		}
		if spent >= s.Cap.MicroUSD {
			return Reply{}, ErrBackupCapped
		}
	}
	r, err := s.Backup.Turn(ctx, req, onText)
	if r.Source == "" {
		r.Source = SourceBedrock
	}
	if !s.Exempt && s.Cap != nil && s.Cap.Spend != nil {
		if cost := r.Cost(req.Price); cost > 0 {
			if aerr := s.Cap.Spend.Add(ctx, day, cost); aerr != nil {
				err = errors.Join(err, fmt.Errorf("model: counting the Bedrock backup's spend: %w", aerr))
			}
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
