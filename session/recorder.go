package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// A Recording hands its Store at most MaxBatch events, and at most
// MaxBatchBytes of bodies, in one Append: within DynamoDB's limits for one
// transaction.
const (
	MaxBatch      = 25
	MaxBatchBytes = 2 << 20
)

// Recorder opens sessions on a Store and records their runs under a Policy.
// The rule is applied when an event is recorded: a Never kind doesn't reach
// the Store, so it can't outlive a crash either.
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

// expires is when a session last touched at now may go, under the policy
// (zero: kept until deleted).
func (r *Recorder) expires(now time.Time) time.Time {
	if keep := r.policy.SessionRetention(); keep > 0 {
		return now.Add(keep)
	}
	return time.Time{}
}

// Open creates a session, a thread for an agent's runs.
func (r *Recorder) Open(ctx context.Context, tenant, agent string, kind Kind) (Session, error) {
	now := r.now().UTC()
	s := Session{ID: newID(), Tenant: tenant, Agent: agent, Kind: kind, Started: now, Updated: now, Expires: r.expires(now)}
	if err := r.store.Create(ctx, s); err != nil {
		return Session{}, err
	}
	return s, nil
}

// Start records a new run in s: nothing may be recorded for the run yet. The
// caller must End or Pause the Recording: that is what stops its writer.
func (r *Recorder) Start(s Session, run string) *Recording {
	return r.recording(s, run, 0)
}

// Resume carries on recording a run that paused, from its last Seq: in this
// process or a fresh one. A run that has ended can't be resumed (ErrEnded).
func (r *Recorder) Resume(ctx context.Context, s Session, run string) (*Recording, error) {
	state, _, err := r.store.ReadRun(ctx, s.Tenant, s.ID, run)
	if err != nil {
		return nil, err
	}
	if state.Ended {
		return nil, ErrEnded
	}
	return r.recording(s, run, state.Seq), nil
}

func (r *Recorder) recording(s Session, run string, seq int64) *Recording {
	rec := &Recording{r: r, s: s, run: run, queued: seq, stored: seq,
		wake: make(chan struct{}, 1), changed: make(chan struct{}), ending: make(chan struct{}, 1)}
	go rec.write()
	return rec
}

// Recording records one run, and is its only writer.
//
// Record never waits: it numbers and queues the event, and one goroutine per
// recording writes the queue to the Store in order, in batches of what queued
// during the last write. Flush, Pause and End wait for it, and every wait
// honours its context.
//
// A failed write loses nothing: its events stay queued, Flush, Pause and End
// return the error, and the next Record, Flush, Pause or End tries again. A
// retry can't store an event twice, because the Store skips a Seq it already
// has. Pause and End can likewise be called again after a timeout or a failed
// Store call.
//
// All methods are safe to call from several goroutines (for example, tool
// calls running in parallel); events keep the order Record saw them in.
type Recording struct {
	r    *Recorder
	s    Session
	run  string
	wake chan struct{} // pokes the writer; holds at most one poke

	mu       sync.Mutex
	pending  []Event       // recorded, not yet stored, in order
	queued   int64         // the last Seq given
	stored   int64         // the last Seq the Store has accepted
	failed   error         // the last write's error, until the next attempt
	closed   bool          // Pause or End was called: no more Record
	finished bool          // the Store has held or ended the run
	changed  chan struct{} // closed and replaced whenever stored or failed changes

	ending chan struct{} // holds one token while a Pause or End runs: one at a time
}

// Session is the session the run is recorded in.
func (rec *Recording) Session() Session { return rec.s }

// Run is the ID of the run this recording records.
func (rec *Recording) Run() string { return rec.run }

// Record queues one event as its kind's rule says. A body over MaxBody is
// recorded as an OmittedBody. Record fails only after Pause or End, or when
// body can't be encoded; write errors come from Flush, Pause and End.
func (rec *Recording) Record(kind EventKind, body any) error {
	rule := rec.r.policy[kind]
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if len(raw) > MaxBody {
		raw, _ = json.Marshal(OmittedBody{Omitted: "over MaxBody", Bytes: len(raw)})
	}
	now := rec.r.now().UTC()
	e := Event{Session: rec.s.ID, Run: rec.run, Time: now, Kind: kind, Body: raw, UntilEnd: rule.Keep == UntilEnd}
	if rule.Keep == Kept && rule.Retention > 0 {
		e.Expires = now.Add(rule.Retention)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.closed {
		return ErrEnded
	}
	if rule.Keep == Never {
		return nil
	}
	rec.queued++
	e.Seq = rec.queued
	rec.pending = append(rec.pending, e)
	rec.poke()
	return nil
}

// Flush waits until every event recorded so far is stored, retrying a write
// that failed.
func (rec *Recording) Flush(ctx context.Context) error {
	rec.mu.Lock()
	finished := rec.finished
	rec.mu.Unlock()
	if finished {
		return ErrEnded
	}
	return rec.flush(ctx)
}

// Pause stores what's queued and holds the run: its session doesn't expire
// until the run ends, and what it recorded stays as it is. The run carries on
// in a Recording from Resume. After an error nothing more can be recorded
// here, and Pause (or End) can be called again to finish. Once it has
// succeeded, Pause and End return ErrEnded.
func (rec *Recording) Pause(ctx context.Context) error {
	return rec.close(ctx, func() error {
		return rec.r.store.Hold(ctx, rec.s.Tenant, rec.s.ID, rec.run)
	})
}

// End stores what's queued and ends the run; events kept only until the end
// are dropped with it, and the session's retention starts again. After an
// error (a timeout, a failed write, a failed Store.EndRun) nothing more can be
// recorded, and End can be called again to finish. Once it has succeeded, End
// returns ErrEnded.
func (rec *Recording) End(ctx context.Context) error {
	return rec.close(ctx, func() error {
		err := rec.r.store.EndRun(ctx, rec.s.Tenant, rec.s.ID, rec.run, rec.r.expires(rec.r.now().UTC()))
		if errors.Is(err, ErrEnded) { // an earlier attempt got through
			return nil
		}
		return err
	})
}

// close stops recording, stores what's queued, then runs finish: the Store's
// Hold or EndRun.
func (rec *Recording) close(ctx context.Context, finish func() error) error {
	select { // wait for another Pause or End, as long as ctx allows
	case rec.ending <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-rec.ending }()
	rec.mu.Lock()
	if rec.finished {
		rec.mu.Unlock()
		return ErrEnded
	}
	rec.closed = true
	rec.mu.Unlock()

	if err := rec.flush(ctx); err != nil {
		return err
	}
	if err := finish(); err != nil {
		return err
	}
	rec.mu.Lock()
	rec.finished = true
	rec.poke() // the writer sees closed with nothing pending, and stops
	rec.mu.Unlock()
	return nil
}

// flush asks for a write and waits until stored reaches what's queued now, or
// that write fails.
func (rec *Recording) flush(ctx context.Context) error {
	rec.mu.Lock()
	target := rec.queued
	rec.failed = nil
	rec.poke()
	for rec.stored < target {
		if rec.failed != nil {
			err := rec.failed
			rec.mu.Unlock()
			return err
		}
		changed := rec.changed
		rec.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
		rec.mu.Lock()
	}
	rec.mu.Unlock()
	return nil
}

// poke wakes the writer without waiting. Called with mu held.
func (rec *Recording) poke() {
	select {
	case rec.wake <- struct{}{}:
	default:
	}
}

// broadcast tells waiters that stored or failed changed. Called with mu held.
func (rec *Recording) broadcast() {
	close(rec.changed)
	rec.changed = make(chan struct{})
}

// write is the recording's writer. When poked, it appends what's pending, a
// batch at a time, until nothing is left or a write fails; then it waits for
// the next poke. It stops once the recording is closed and nothing is pending.
func (rec *Recording) write() {
	ctx := context.Background() // a write outlives any one caller; Flush, Pause and End bound the waits
	for range rec.wake {
		for {
			rec.mu.Lock()
			if len(rec.pending) == 0 {
				done := rec.closed
				rec.mu.Unlock()
				if done {
					return
				}
				break
			}
			n, size := 0, 0
			for _, e := range rec.pending[:min(len(rec.pending), MaxBatch)] {
				if n > 0 && size+len(e.Body) > MaxBatchBytes {
					break
				}
				n, size = n+1, size+len(e.Body)
			}
			batch := rec.pending[:n:n] // Record appends after it, never into it
			rec.mu.Unlock()

			err := rec.r.store.Append(ctx, rec.s.Tenant, rec.s.ID, rec.run, batch)

			rec.mu.Lock()
			if err != nil {
				rec.failed = err
				rec.broadcast()
				rec.mu.Unlock()
				break // the batch stays pending; the next poke retries it
			}
			rec.pending = rec.pending[n:]
			rec.stored = batch[n-1].Seq
			rec.broadcast()
			rec.mu.Unlock()
		}
	}
}

// Cost sums the cost of Usage events, in millionths of a dollar.
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
