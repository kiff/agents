package runner

import (
	"testing"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
)

// A run goes on where it started; one with turns from before runs kept it
// stays on the Claude API, so a run under way never moves to the backup.
func TestARunGoesOnWhereItStarted(t *testing.T) {
	acted := &run.Turn{Reply: model.Message{Role: model.Assistant, Blocks: []model.Block{{Kind: model.ToolUse, CallID: "c1", Tool: "stripe_refund"}}}}
	for _, c := range []struct {
		r    run.Run
		want string
	}{
		{run.Run{}, ""},
		{run.Run{Turns: 3}, model.SourceAnthropic},
		{run.Run{Turns: 3, ModelSource: model.SourceBedrock}, model.SourceBedrock},
		{run.Run{ModelSource: model.SourceAnthropic}, model.SourceAnthropic},
		// Carried on from a crash: the turn it acted in is stored with where
		// it ran; one stored before runs kept it ran on the Claude API.
		{run.Run{Paused: acted, Resumes: 1, ModelSource: model.SourceBedrock}, model.SourceBedrock},
		{run.Run{Paused: acted, Resumes: 1}, model.SourceAnthropic},
		// Restarted from its start: none of its earlier turns is sent.
		{run.Run{Paused: &run.Turn{}, Resumes: 1}, ""},
	} {
		if got := sourceOf(c.r); got != c.want {
			t.Errorf("sourceOf(%+v) = %q, want %q", c.r, got, c.want)
		}
	}
}
