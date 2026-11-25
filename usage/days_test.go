package usage_test

import (
	"testing"

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
