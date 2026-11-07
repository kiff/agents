package server_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/server"
	"github.com/kiff/agents/store/memory"
)

// A chat's AI bound is no more: KIFF's balance is asked as the run starts.
// A tafs cloud from before still sends it, valid or not, and it's ignored:
// the chat starts, and nothing is counted toward a period. The period's
// read still answers, for apps from before, and erasing the account erases
// what it kept.
func TestAChatsAIBoundIsIgnored(t *testing.T) {
	f := newRunFixture(t)
	use := memory.NewUsage()
	f.server.Usage = use
	f.restart()
	f.putAgent("t1", "refunds")
	if err := use.Add(ctx, "t1", "2026-10-20", 4_850_000, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, ai := range []map[string]any{
		{"period": "2026-10-20", "bound_micro_usd": 4_850_000},
		{"period": "Oct 20", "bound_micro_usd": -1},
	} {
		code, body := f.do("t1", "POST", "/agents/refunds/runs", map[string]any{"input": "x", "background": true, "ai": ai})
		f.want(202, code, body, "a chat past its old bound")
	}
	for _, s := range f.runner.starts {
		if s.Meter != "" {
			t.Fatalf("a chat counted toward %q", s.Meter)
		}
	}
	code, body := f.do("t1", "GET", "/usage/ai?period=2026-10-20", nil)
	f.want(200, code, body, "reading the period")
	code, body = f.do("t1", "DELETE", "/tenant", nil)
	f.want(204, code, body, "erasing the tenant")
	if p, _ := use.Get(ctx, "t1", "2026-10-20"); p.Spent != 0 {
		t.Fatalf("t1's period kept: %+v", p)
	}
}

// A run KIFF's balance refuses is a 409 with KIFF's code: the account's tafs
// wait for credits. A code it doesn't know reads as credits used up. KIFF
// not answering is a 502, as when it doesn't say what a person may do.
func TestACreditsRefusal(t *testing.T) {
	for _, c := range []struct {
		err        error
		status     int
		code, says string
	}{
		{&server.CreditsRefusal{Why: "credits_used_up"}, 409, "credits_used_up", "wait for credits"},
		{&server.CreditsRefusal{Why: "limit_reached"}, 409, "limit_reached", "wait for credits"},
		{&server.CreditsRefusal{Why: "no_plan"}, 409, "no_plan", "wait for credits"},
		{&server.CreditsRefusal{Why: "something_new"}, 409, "credits_used_up", "wait for credits"},
		{server.KIFFUnanswered("KIFF didn't say whether a run may start"), 502, "kiff_unanswered", "KIFF didn't say"},
	} {
		f := newRunFixture(t)
		f.putAgent("t1", "refunds")
		f.runner.refusing = c.err
		code, body := f.do("t1", "POST", "/agents/refunds/runs", map[string]any{"input": "x"})
		if msg, _ := body["message"].(string); code != c.status || body["error"] != c.code || !strings.Contains(msg, c.says) {
			t.Errorf("%v: %d %v", c.err, code, body)
		}
	}
}
