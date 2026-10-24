package runner

import (
	"encoding/json"
	"testing"

	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
)

// A run goes fast when its agent is set to, someone waits on it, and its
// conversation began fast and stayed so: a conversation never changes speed.
func TestARunGoesFastOnlyWhenSomeoneWaitsAndItsConversationDid(t *testing.T) {
	usage := func(speed string) session.Event {
		b, _ := json.Marshal(session.UsageBody{Model: "m", Speed: speed})
		return session.Event{Kind: session.Usage, Body: b}
	}
	fastDef := registry.Definition{Speed: registry.SpeedFast}
	for _, c := range []struct {
		name    string
		d       registry.Definition
		trigger run.Trigger
		events  []session.Event
		want    bool
	}{
		{"a new conversation with the owner", fastDef, run.ByOwner, nil, true},
		{"a trial", fastDef, run.ByTrial, nil, true},
		{"an eval case", fastDef, run.ByEval, nil, true},
		{"a schedule", fastDef, run.BySchedule, nil, false},
		{"an event", fastDef, run.ByEvent, nil, false},
		{"an agent not set to", registry.Definition{}, run.ByOwner, nil, false},
		{"a conversation that went fast", fastDef, run.ByOwner, []session.Event{usage("fast")}, true},
		{"one that fell back", fastDef, run.ByOwner, []session.Event{usage("fast"), usage("standard")}, false},
		{"one from before speeds", fastDef, run.ByOwner, []session.Event{usage("")}, false},
	} {
		if got := fast(c.d, c.trigger, c.events); got != c.want {
			t.Errorf("%s: fast %v", c.name, got)
		}
	}
}
