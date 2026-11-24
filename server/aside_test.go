package server_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kiff/agents/server"
)

// What the person had open goes only with their own message, with words, and no larger than MaxAside: a door's
// message, a page with no words, and a page past the bound are refused, and nothing starts.
func TestOnlyAPersonsOwnMessageCarriesWhatTheyHadOpen(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	ana := map[string]any{"kind": "member", "id": "ana"}
	for name, body := range map[string]map[string]any{
		"a door's message": {"input": "hi", "aside": "a page", "requester": map[string]any{"kind": "door", "id": "d1"}},
		"no words":         {"input": "", "aside": "a page", "requester": ana},
		"past the bound":   {"input": "hi", "aside": strings.Repeat("x", server.MaxAside+1), "requester": ana},
	} {
		body["background"] = true
		code, out := f.do("t1", "POST", "/agents/payouts/runs", body)
		f.want(400, code, out, name)
	}
	// A person's own message with what they had open starts, as any message does.
	code, out := f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "What's this page?", "aside": "a page", "background": true, "requester": ana})
	f.want(202, code, out, "a person's message with a page")
	if raw, _ := json.Marshal(out); strings.Contains(string(raw), "a page") {
		t.Fatalf("the run's view shows what the person had open: %s", out)
	}
}
