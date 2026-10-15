package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

// MaxBatch is the most events a Recording hands its Store in one Append.
const MaxBatch = 25

// Recorder starts sessions on a Store and records them under a Policy. The rule
// is applied when an event is recorded: a Never kind doesn't reach the Store,
// so it can't outlive a crash either.
type Recorder struct {
	store  Store
	policy Policy
	now    func() time.Time
}

// NewRecorder checks the policy and returns a recorder for store.
func NewRecorder(store Store, policy Policy) (*Recorder, error) {
	return NewRecorderAt(store, policy, time.Now)
}

// NewRecorderAt is NewRecorder with its own clock, for tests and replays.
func NewRecorderAt(store Store, policy Policy, now func() time.Time) (*Recorder, error) {
	if err := policy.Check(); err != nil {
		return nil, err
	}
	return &Recorder{store: store, policy: policy, now: now}, nil
}

// Start creates a running session and returns the Recording that records it. The
// caller must End the run: that is what stops its writer.
func (r *Recorder) Start(ctx context.Context, tenant, agent string, kind Kind) (*Recording, error) {
	now := r.now().UTC()
	s := Session{ID: newID(), Tenant: tenant, Agent: agent, Kind: kind, Status: Running, Started: now, Updated: now}
	if err := r.store.Create(ctx, s); err != nil {
		return nil, err
	}
	run := &Recording{
		r:     r,
		s:     s,
		queue: make(chan item, 4*MaxBatch),
	}
	go run.write(context.WithoutCancel(ctx))
	return run, nil
}

// Recording records one session. Record doesn't wait for the Store: one goroutine
// per recording writes events in the order they were recorded, in batches of what
// queued while the last write was in flight. Flush and End wait for it.
//
// Record, Flush and End are safe to call from several goroutines (for example,
// tool calls running in parallel); events then keep the order Record saw them.
type Recording struct {
	r     *Recorder
	s     Session
	queue chan item

	mu    sync.Mutex // orders sends on queue with closing it
	ended bool

	// failed holds the first write error; every later call returns it. The
	// writer sets it without mu, so a Record blocked on a full queue can't
	// hold the writer up.
	failed atomic.Pointer[error]
}

// item is an event to write, or, with done set, a flush marker.
type item struct {
	e    Event
	done chan error
}

// Session is the session this run records.
func (run *Recording) Session() Session { return run.s }

// Record queues one event as its kind's rule says. It returns ErrEnded after
// End, and the error of an earlier write that failed.
func (run *Recording) Record(kind EventKind, body any) error {
	rule := run.r.policy[kind]
	if rule.Keep == Never {
		return run.err()
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	now := run.r.now().UTC()
	e := Event{Session: run.s.ID, Time: now, Kind: kind, Body: raw, UntilEnd: rule.Keep == UntilEnd}
	if rule.Keep == Kept && rule.Retention > 0 {
		e.Expires = now.Add(rule.Retention)
	}
	if err := run.err(); err != nil {
		return err
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.ended {
		return ErrEnded
	}
	run.queue <- item{e: e}
	return nil
}

// Flush waits until every event recorded so far is stored.
func (run *Recording) Flush(ctx context.Context) error {
	done := make(chan error, 1)
	run.mu.Lock()
	if run.ended {
		run.mu.Unlock()
		return ErrEnded
	}
	run.queue <- item{done: done}
	run.mu.Unlock()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// End stores what's queued, stops the writer and ends the session. Events kept
// only until the end are dropped with it.
func (run *Recording) End(ctx context.Context, status Status) error {
	done := make(chan error, 1)
	run.mu.Lock()
	if run.ended {
		run.mu.Unlock()
		return ErrEnded
	}
	run.ended = true
	run.queue <- item{done: done}
	close(run.queue)
	run.mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	return run.r.store.End(ctx, run.s.Tenant, run.s.ID, status)
}

func (run *Recording) err() error {
	if p := run.failed.Load(); p != nil {
		return *p
	}
	return nil
}

// write is the run's writer: it takes what's queued, up to MaxBatch events,
// appends it in one call, and answers flush markers once everything before
// them is stored.
func (run *Recording) write(ctx context.Context) {
	batch := make([]Event, 0, MaxBatch)
	var markers []chan error
	for first := range run.queue {
		batch, markers = batch[:0], markers[:0]
		add := func(it item) {
			if it.done != nil {
				markers = append(markers, it.done)
			} else {
				batch = append(batch, it.e)
			}
		}
		add(first)
	drain:
		for len(batch) < MaxBatch && len(markers) == 0 {
			select {
			case it, ok := <-run.queue:
				if !ok {
					break drain
				}
				add(it)
			default:
				break drain
			}
		}
		err := run.err()
		if err == nil && len(batch) > 0 {
			if err = run.r.store.Append(ctx, run.s.Tenant, run.s.ID, batch); err != nil {
				run.failed.CompareAndSwap(nil, &err)
			}
		}
		for _, done := range markers {
			done <- err
		}
	}
}

// Cost sums the cost of a session's Usage events, in millionths of a dollar.
func Cost(events []Event) (int64, error) {
	var total int64
	for _, e := range events {
		if e.Kind != Usage {
			continue
		}
		var u UsageBody
		if err := json.Unmarshal(e.Body, &u); err != nil {
			return 0, err
		}
		total += u.CostMicroUSD
	}
	return total, nil
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b)
}
