package memory_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/store/memory"
	toolmemory "github.com/kiff/agents/tool/memory"
)

// kiff/tafs#107: a trial reads the taf's notes but changes none.
func TestATrialReadsNotesAndChangesNone(t *testing.T) {
	ctx := context.Background()
	notes := memory.New()
	m := toolmemory.New(notes, "t1", "rae")
	if r, err := m.Call(ctx, json.RawMessage(`{"action":"write","name":"rule","text":"Return first."}`)); err != nil || r.IsError {
		t.Fatalf("write: %+v, %v", r, err)
	}
	trial := run.WithTrial(ctx, run.Trial{Draft: "drf_2"})
	for _, in := range []string{`{"action":"write","name":"rule","text":"Refund at once."}`, `{"action":"forget","name":"rule"}`} {
		if r, err := m.Call(trial, json.RawMessage(in)); err != nil || !strings.Contains(r.Text, "trial") {
			t.Fatalf("%s in a trial: %+v, %v", in, r, err)
		}
	}
	got, err := m.Call(trial, json.RawMessage(`{"action":"list"}`))
	if err != nil || !strings.Contains(got.Text, "Return first.") {
		t.Fatalf("list in a trial: %+v, %v; want the note unchanged", got, err)
	}
}
