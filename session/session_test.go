package session_test

import (
	"context"
	"testing"
	"time"

	"github.com/kiff/agents/session"
	"github.com/kiff/agents/session/sessiontest"
)

func TestMemory(t *testing.T) {
	sessiontest.Run(t, func() sessiontest.Backend { return session.NewMemory() })
}

func TestPolicyNeedsEveryKind(t *testing.T) {
	p := session.TafsDefault(time.Hour)
	if err := p.Check(); err != nil {
		t.Fatal(err)
	}
	delete(p, session.Usage)
	if _, err := session.NewRecorder(session.NewMemory(), p); err == nil {
		t.Fatal("a policy without a rule for usage was accepted")
	}
}

func TestPolicyRefusesRetentionOnUnkept(t *testing.T) {
	p := session.TafsDefault(0)
	p[session.ToolResult] = session.Rule{Keep: session.UntilEnd, Retention: time.Hour}
	if err := p.Check(); err == nil {
		t.Fatal("retention on an until-end kind was accepted")
	}
	p[session.ToolResult] = session.Rule{Keep: session.Kept, Retention: -time.Hour}
	if err := p.Check(); err == nil {
		t.Fatal("negative retention was accepted")
	}
}

func TestTafsDefaultKeepsToolResultsOnlyForTheRun(t *testing.T) {
	p := session.TafsDefault(30 * 24 * time.Hour)
	if p[session.ToolResult].Keep != session.UntilEnd {
		t.Fatalf("tool results: %+v", p[session.ToolResult])
	}
	if p[session.OwnerMessage] != (session.Rule{Keep: session.Kept, Retention: 30 * 24 * time.Hour}) {
		t.Fatalf("owner messages: %+v", p[session.OwnerMessage])
	}
}

// Recording an event is on every step of the run loop: it has to stay cheap.
func BenchmarkRecord(b *testing.B) {
	ctx := context.Background()
	r, err := session.NewRecorder(session.NewMemory(), session.TafsDefault(time.Hour))
	if err != nil {
		b.Fatal(err)
	}
	s, err := r.Start(ctx, "t1", "refunds", session.Run)
	if err != nil {
		b.Fatal(err)
	}
	body := map[string]string{"tool": "list_charges", "args": `{"limit":10}`}
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := r.Record(ctx, s, session.ToolCall, body); err != nil {
			b.Fatal(err)
		}
	}
}
