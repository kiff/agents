package run

import (
	"context"
	"errors"
	"time"
)

// Reporting is runs, reporting each run that ends having used AI (its Cost)
// to KIFF once: on the Put that moves it to a final status, from whichever
// path (the run itself, a cancel, the sweep). KIFF keeps one row per run and
// works out its credits from the report, so a run's usage outlives the run,
// which its agent's keep rules may delete. A report that fails never fails
// the Put: it's kept on the run (UsageTries, UsageRetry), and the sweep
// sends it again (Again) after each of UsageWaits, until KIFF takes it.
type Reporting struct {
	Runs
	// Report sends r's usage report. KIFF keeps a run's first report, so
	// sending one again changes nothing.
	Report func(ctx context.Context, r Run) error
	// Log, when set, hears of a report that failed.
	Log func(msg string, err error)
	// Now is the clock a kept report's next try is set by; nil is time.Now.
	Now func() time.Time
}

// UsageWaits is how long a usage report KIFF didn't take waits before each
// of its tries again, as a note does. Past the last it's given up, and the
// run's usage isn't counted.
var UsageWaits = [...]time.Duration{5 * time.Minute, 20 * time.Minute, time.Hour}

// Reported reports whether a run as r is reported: it ended, having used AI.
// One that used none (refused before its first turn) isn't counted, so it
// isn't reported.
func Reported(r Run) bool { return r.Status.Final() && r.Cost > 0 }

// Put is Runs.Put, then the report when next has just ended.
func (rp Reporting) Put(ctx context.Context, prev, next Run) error {
	if err := rp.Runs.Put(ctx, prev, next); err != nil {
		return err
	}
	if rp.Report == nil || prev.Status.Final() || !Reported(next) {
		return nil
	}
	ctx = context.WithoutCancel(ctx)
	if err := rp.Report(ctx, next); err != nil {
		rp.log("run: a usage report was not sent; it's kept and sent again", err)
		_ = rp.keep(ctx, next, 1)
	}
	return nil
}

// Again sends r's kept report again, from the sweep. Taken, it's no longer
// kept; not taken, it waits for its next try, or is given up past the last.
func (rp Reporting) Again(ctx context.Context, r Run) error {
	if rp.Report == nil || !Reported(r) {
		return rp.keep(ctx, r, 0)
	}
	if err := rp.Report(ctx, r); err != nil {
		if r.UsageTries >= len(UsageWaits) {
			rp.log("run: a usage report was given up after its last try", err)
			return rp.keep(ctx, r, 0)
		}
		rp.log("run: a usage report was not sent again; it's kept", err)
		return rp.keep(ctx, r, r.UsageTries+1)
	}
	return rp.keep(ctx, r, 0)
}

// keep stores r's report as owed for its tries-th time (0: not owed). A run
// changed since it was read (its note kept on the same Put, say) is read
// again and kept as it is now, so neither loses the other's retry; a run
// deleted since is left alone.
func (rp Reporting) keep(ctx context.Context, r Run, tries int) error {
	now := time.Now
	if rp.Now != nil {
		now = rp.Now
	}
	for range keepTries {
		next := r.clone()
		next.UsageTries, next.UsageRetry = tries, time.Time{}
		if tries > 0 {
			next.UsageRetry = now().UTC().Add(UsageWaits[tries-1])
		}
		if next.UsageTries == r.UsageTries && next.UsageRetry.Equal(r.UsageRetry) {
			return nil
		}
		next.Version, next.Updated = r.Version+1, now().UTC()
		err := rp.Runs.Put(ctx, r, next)
		switch {
		case errors.Is(err, ErrNotFound):
			return nil
		case !errors.Is(err, ErrConflict):
			return err
		}
		fresh, err := rp.Runs.Get(ctx, r.Tenant, r.ID)
		switch {
		case errors.Is(err, ErrNotFound):
			return nil
		case err != nil:
			return err
		}
		r = fresh
	}
	return nil
}

// keepTries is how many times keep reads a run that moved on again.
const keepTries = 3

func (rp Reporting) log(msg string, err error) {
	if rp.Log != nil {
		rp.Log(msg, err)
	}
}
