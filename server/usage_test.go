package server_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/store/memory"
)

// A chat that names its period is counted, and refused past its bound
// (kiff/tafs#193). Only starting is refused; the owner's go on is KIFF's,
// and tafs cloud then sends no bound.
func TestAIFairUse(t *testing.T) {
	f := newRunFixture(t)
	use := memory.NewUsage()
	f.server.Usage = use
	f.restart()
	f.putAgent("t1", "refunds")
	ai := func(bound int64) map[string]any {
		return map[string]any{"input": "x", "background": true, "ai": map[string]any{"period": "2026-10-20", "bound_micro_usd": bound}}
	}

	code, body := f.do("t1", "POST", "/agents/refunds/runs", ai(4_850_000))
	f.want(202, code, body, "a chat under its bound")
	if n := len(f.runner.starts); n != 1 || f.runner.starts[0].Meter != "2026-10-20" {
		t.Fatalf("starts %+v: want one, metered to its period", f.runner.starts)
	}

	if err := use.Add(ctx, "t1", "2026-10-20", 4_850_000, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	code, body = f.do("t1", "POST", "/agents/refunds/runs", ai(4_850_000))
	f.want(409, code, body, "a chat at its bound")
	if msg, _ := body["error"].(string); !strings.HasPrefix(msg, "ai_fair_use:") {
		t.Fatalf("error %q: want it to start with ai_fair_use:", msg)
	}
	if n := len(f.runner.starts); n != 1 {
		t.Fatalf("a refused chat started: %d starts", n)
	}

	// Another tenant, or the next period, isn't held back by it.
	f.putAgent("t2", "refunds")
	code, body = f.do("t2", "POST", "/agents/refunds/runs", ai(4_850_000))
	f.want(202, code, body, "another tenant's chat")
	next := map[string]any{"input": "x", "background": true, "ai": map[string]any{"period": "2026-11-20", "bound_micro_usd": 4_850_000}}
	code, body = f.do("t1", "POST", "/agents/refunds/runs", next)
	f.want(202, code, body, "a chat in the next period")

	code, body = f.do("t1", "GET", "/usage/ai?period=2026-10-20", nil)
	f.want(200, code, body, "reading the period")
	if body["spent_micro_usd"] != float64(4_850_000) {
		t.Fatalf("period %v", body)
	}

	// No bound counts without refusing: our own accounts.
	code, body = f.do("t2", "POST", "/agents/refunds/runs", map[string]any{"input": "x", "background": true, "ai": map[string]any{"period": "2026-10-20", "bound_micro_usd": 0}})
	f.want(202, code, body, "a chat with no bound")
	// Without ai, a chat isn't counted, as before.
	code, body = f.do("t1", "POST", "/agents/refunds/runs", map[string]any{"input": "x", "background": true})
	f.want(202, code, body, "a chat that doesn't ask to be counted")
	if s := f.runner.starts[len(f.runner.starts)-1]; s.Meter != "" {
		t.Fatalf("an uncounted chat has meter %q", s.Meter)
	}

	// Erasing the account erases its periods.
	code, body = f.do("t1", "DELETE", "/tenant", nil)
	f.want(204, code, body, "erasing the tenant")
	if p, _ := use.Get(ctx, "t1", "2026-10-20"); p.Spent != 0 {
		t.Fatalf("t1's period kept: %+v", p)
	}
}

func TestAIFairUseRefusesABadPeriodOrNoStore(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "refunds")
	for _, ai := range []map[string]any{
		{"period": "", "bound_micro_usd": 1},
		{"period": "Oct 20", "bound_micro_usd": 1},
		{"period": strings.Repeat("a", 65), "bound_micro_usd": 1},
		{"period": "2026-10-20", "bound_micro_usd": -1},
	} {
		code, body := f.do("t1", "POST", "/agents/refunds/runs", map[string]any{"input": "x", "background": true, "ai": ai})
		f.want(400, code, body, "a bad ai")
	}
	// A server that doesn't count refuses a chat that asks to be: never
	// started uncounted.
	code, body := f.do("t1", "POST", "/agents/refunds/runs", map[string]any{"input": "x", "background": true, "ai": map[string]any{"period": "2026-10-20", "bound_micro_usd": 1}})
	f.want(503, code, body, "a counted chat on a server with no usage store")
	if n := len(f.runner.starts); n != 0 {
		t.Fatalf("started %d", n)
	}
}
