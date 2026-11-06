package runner

import (
	"testing"

	"github.com/kiff/agents/model"
)

// What changed since a continued run's preface was first sent: new lines,
// then lines that went away with nothing in their place, never the old date
// (qa-kiff on #360: a state note that went away was still read as standing).
func TestSinceSaysWhatIsNewAndWhatWentAway(t *testing.T) {
	text := func(s string) model.Block { return model.Block{Kind: model.Text, Text: s} }
	state := StateNote + "\n\nDrafts in this conversation, as kept now:\n- drf_1, for agent refunds-a1: submitted; not claimed yet\n"
	first := []model.Block{text(state), text("Today is Friday 6 November 2026 (2026-11-06), 12:10 UTC.")}
	if got := since(first, first); got != nil {
		t.Fatalf("nothing changed, yet: %+v", got)
	}
	later := text("Today is Friday 6 November 2026 (2026-11-06), 12:41 UTC.")
	got := since(first, []model.Block{later})
	want := SinceNote + "\n\n" + later.Text + "\n\n" + GoneNote + "\n\n" + state
	if len(got) != 1 || got[0].Text != want {
		t.Fatalf("since: %+v\nwant %q", got, want)
	}
	if got := since(first, []model.Block{text(state), later}); len(got) != 1 || got[0].Text != SinceNote+"\n\n"+later.Text {
		t.Fatalf("only the date changed: %+v", got)
	}
}
