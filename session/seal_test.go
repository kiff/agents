package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
)

func idle() session.Rule {
	return session.Rule{Keep: session.UntilIdle, Retention: time.Hour, Max: 4 * time.Hour}
}

func TestOnlyAKeptReadIsKeptUntilIdle(t *testing.T) {
	for _, c := range []struct {
		name string
		kind session.EventKind
		rule session.Rule
		ok   bool
	}{
		{"a kept read, an hour idle, four at most", session.KeptRead, idle(), true},
		{"a kept read left unwritten", session.KeptRead, session.Rule{Keep: session.Never}, true},
		{"a kept read never kept for good", session.KeptRead, session.Rule{Keep: session.Kept, Retention: time.Hour}, false},
		{"no idle window", session.KeptRead, session.Rule{Keep: session.UntilIdle, Max: time.Hour}, false},
		{"a max shorter than the idle window", session.KeptRead, session.Rule{Keep: session.UntilIdle, Retention: time.Hour, Max: time.Minute}, false},
		{"a tool result can't be kept until idle", session.ToolResult, idle(), false},
		{"a max on a kept rule", session.AgentMessage, session.Rule{Keep: session.Kept, Retention: time.Hour, Max: time.Hour}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := session.DefaultPolicy(24 * time.Hour)
			p[c.kind] = c.rule
			if c.kind == session.ToolResult {
				p[session.ToolCall] = c.rule
			}
			if err := p.Check(); (err == nil) != c.ok {
				t.Fatalf("Check() = %v, want ok %v", err, c.ok)
			}
		})
	}
	if r := session.DefaultPolicy(time.Hour).Rule(session.KeptRead); r.Keep != session.Never {
		t.Fatalf("a policy that leaves kept reads out writes them: %+v", r)
	}
}

func TestKeySealerOpensOnlyWhereItSealed(t *testing.T) {
	s := &session.KeySealer{Keys: session.MemoryKeys()}
	sealed, err := s.Seal(ctx, "t1", []byte("here"), []byte("a charge"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "a charge") {
		t.Fatal("sealed in clear")
	}
	if got, err := s.Open(ctx, "t1", []byte("here"), sealed); err != nil || string(got) != "a charge" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	if _, err := s.Open(ctx, "t1", []byte("elsewhere"), sealed); err == nil {
		t.Fatal("opened with another binding")
	}
	if _, err := s.Open(ctx, "t2", []byte("here"), sealed); err == nil {
		t.Fatal("opened under another tenant's key")
	}
	failing := &session.KeySealer{Keys: func(context.Context, string) ([]byte, error) { return nil, errors.New("down") }}
	if _, err := failing.Seal(ctx, "t1", nil, []byte("x")); err == nil {
		t.Fatal("sealed without a key")
	}
}

func TestARecordingSealsWhatItKeepsUntilIdleInAChatOnly(t *testing.T) {
	store := memory.New()
	p := session.DefaultPolicy(24 * time.Hour)
	p[session.KeptRead] = idle()
	sealer := &session.KeySealer{Keys: session.MemoryKeys()}
	rec, err := session.NewRecorder(store, p)
	if err != nil {
		t.Fatal(err)
	}
	rec.Sealing(sealer)
	body := session.KeptReadBody{CallID: "c1", Tool: "get_charge", Input: json.RawMessage(`{"charge":"ch_1"}`), Result: "50.00 EUR"}

	record := func(kind session.Kind) (session.Session, []session.Event) {
		s, err := rec.Open(ctx, "t1", "rae", kind)
		if err != nil {
			t.Fatal(err)
		}
		r, err := rec.Start(ctx, s, "run1", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Record(session.KeptRead, body); err != nil {
			t.Fatal(err)
		}
		if err := r.End(ctx); err != nil {
			t.Fatal(err)
		}
		_, events, err := store.Read(ctx, "t1", s.ID)
		if err != nil {
			t.Fatal(err)
		}
		return s, events
	}

	s, events := record(session.KindChat)
	if len(events) != 1 {
		t.Fatalf("a chat kept %d events after its run ended, want its read", len(events))
	}
	e := events[0]
	// Stored sealed: one "sealed" field and nothing in clear. The check reads
	// the JSON, not its text: base64 can spell "EUR" by chance.
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(e.Body, &stored); err != nil || len(stored) != 1 || stored["sealed"] == nil {
		t.Fatalf("stored in clear: %s", e.Body)
	}
	if strings.Contains(string(e.Body), `"result"`) || strings.Contains(string(e.Body), `"charge"`) {
		t.Fatalf("stored in clear: %s", e.Body)
	}
	plain, err := session.OpenEvent(ctx, sealer, "t1", e)
	if err != nil {
		t.Fatal(err)
	}
	var got session.KeptReadBody
	if err := json.Unmarshal(plain, &got); err != nil || got.Result != "50.00 EUR" {
		t.Fatalf("opened %s, %v", plain, err)
	}
	moved := e
	moved.Session = "another"
	if _, err := session.OpenEvent(ctx, sealer, "t1", moved); err == nil {
		t.Fatal("a sealed read opened in another session")
	}
	_ = s

	for _, kind := range []session.Kind{session.KindRun, session.KindTrial} {
		if _, events := record(kind); len(events) != 0 {
			t.Fatalf("a %s session kept %d reads", kind, len(events))
		}
	}
}

func TestARecordingWithoutASealerKeepsNoRead(t *testing.T) {
	store := memory.New()
	p := session.DefaultPolicy(24 * time.Hour)
	p[session.KeptRead] = idle()
	rec, _ := session.NewRecorder(store, p)
	s, _ := rec.Open(ctx, "t1", "rae", session.KindChat)
	r, _ := rec.Start(ctx, s, "run1", time.Now())
	_ = r.Record(session.KeptRead, session.KeptReadBody{Tool: "get_charge", Result: "x"})
	if err := r.End(ctx); err != nil {
		t.Fatal(err)
	}
	if _, events, _ := store.Read(ctx, "t1", s.ID); len(events) != 0 {
		t.Fatalf("kept %d reads with nothing to seal them", len(events))
	}
}

func TestASealingFailureIsRetriedAndLosesNothing(t *testing.T) {
	store := memory.New()
	p := session.DefaultPolicy(24 * time.Hour)
	p[session.KeptRead] = idle()
	var down atomic.Bool
	down.Store(true)
	keys := session.MemoryKeys()
	sealer := &session.KeySealer{Keys: func(ctx context.Context, tenant string) ([]byte, error) {
		if down.Load() {
			return nil, errors.New("secrets down")
		}
		return keys(ctx, tenant)
	}}
	rec, _ := session.NewRecorder(store, p)
	rec.Sealing(sealer)
	s, _ := rec.Open(ctx, "t1", "rae", session.KindChat)
	r, _ := rec.Start(ctx, s, "run1", time.Now())
	_ = r.Record(session.KeptRead, session.KeptReadBody{Tool: "get_charge", Result: "x"})
	if err := r.Flush(ctx); err == nil {
		t.Fatal("a flush that couldn't seal succeeded")
	}
	down.Store(false)
	if err := r.End(ctx); err != nil {
		t.Fatal(err)
	}
	if _, events, _ := store.Read(ctx, "t1", s.ID); len(events) != 1 {
		t.Fatalf("after the retry, %d reads kept, want 1", len(events))
	}
}
