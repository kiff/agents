package run

import "context"

// trialKey keys a trial's mark on a context.
type trialKey struct{}

// Trial is a trial run's mark (kiff/tafs#107), on the context its tools are
// called with: set by the runner from the stored run, never by the model.
type Trial struct {
	// Draft is the KIFF draft whose Cards decide the trial's writes; empty
	// is the live Cards.
	Draft string
}

// WithTrial marks ctx as a trial's.
func WithTrial(ctx context.Context, t Trial) context.Context {
	return context.WithValue(ctx, trialKey{}, t)
}

// TrialFrom is the trial ctx is marked with, and whether it is one.
func TrialFrom(ctx context.Context) (Trial, bool) {
	t, ok := ctx.Value(trialKey{}).(Trial)
	return t, ok
}
