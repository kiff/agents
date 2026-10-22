package usage_test

import (
	"testing"

	"github.com/kiff/agents/usage"
)

func TestAllowed(t *testing.T) {
	for _, c := range []struct {
		p     usage.Period
		bound int64
		want  bool
	}{
		{usage.Period{}, 4_850_000, true},
		{usage.Period{Spent: 4_849_999}, 4_850_000, true},
		{usage.Period{Spent: 4_850_000}, 4_850_000, false},
		{usage.Period{Spent: 9_000_000}, 0, true}, // no bound: go on in KIFF, or our own accounts
	} {
		if got := usage.Allowed(c.p, c.bound); got != c.want {
			t.Errorf("Allowed(%+v, %d) = %v, want %v", c.p, c.bound, got, c.want)
		}
	}
}
