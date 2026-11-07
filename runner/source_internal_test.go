package runner

import (
	"testing"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
)

// A run goes on where it started; one with turns from before runs kept it
// stays on the Claude API, so a run under way never moves to the backup.
func TestARunGoesOnWhereItStarted(t *testing.T) {
	for _, c := range []struct {
		r    run.Run
		want string
	}{
		{run.Run{}, ""},
		{run.Run{Turns: 3}, model.SourceAnthropic},
		{run.Run{Turns: 3, ModelSource: model.SourceBedrock}, model.SourceBedrock},
		{run.Run{ModelSource: model.SourceAnthropic}, model.SourceAnthropic},
	} {
		if got := sourceOf(c.r); got != c.want {
			t.Errorf("sourceOf(%+v) = %q, want %q", c.r, got, c.want)
		}
	}
}
