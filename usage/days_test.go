package usage_test

import (
	"testing"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/usage"
)

// Credits is KIFF's meter.Credits, pinned on KIFF's own examples
// (kiff/cloud api/internal/meter/runs_test.go): only KIFF's is charged, and
// this one must measure the same.
func TestCreditsAreKIFFs(t *testing.T) {
	for _, c := range []struct{ micro, want int64 }{
		{0, 0}, {-5, 0}, {1, 1}, {2_700, 1}, {2_701, 2}, {10_800, 4}, {30_000, 12}, {120_000, 45}, {320_000, 119}, {10_800_000, 4_000},
	} {
		if got := usage.Credits(c.micro); got != c.want {
			t.Errorf("Credits(%d) = %d, want %d", c.micro, got, c.want)
		}
	}
}

// Charged is KIFF's meter.ClassifyRun short of Setup: a trial and every ERROR
// are free.
func TestChargedIsKIFFs(t *testing.T) {
	for _, c := range []struct {
		status  run.Status
		trigger run.Trigger
		want    bool
	}{
		{run.Completed, run.ByOwner, true},
		{run.Cancelled, run.BySchedule, true},
		{run.Error, run.ByOwner, false},
		{run.Completed, run.ByTrial, false},
		{run.Error, run.ByTrial, false},
	} {
		if got := usage.Charged(c.status, c.trigger); got != c.want {
			t.Errorf("Charged(%s, %s) = %v, want %v", c.status, c.trigger, got, c.want)
		}
	}
}
