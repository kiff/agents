package main

import (
	"context"
	"strings"
	"testing"

	"github.com/kiff/agents/model"
)

type pricedTurns struct{ usage model.Usage }

func (p pricedTurns) Turn(context.Context, model.Request, func(string)) (model.Reply, error) {
	return model.Reply{Usage: p.usage}, nil
}

// Every turn of a chat is counted at its own request's price, and the line
// goes to the private transcript with the judge's share beside it.
func TestAChatsCostCountsEveryTurnAndTheJudge(t *testing.T) {
	price := model.Price{Input: 2_000_000, Output: 10_000_000, CacheRead: 100_000, CacheWrite: 2_500_000}
	m := &spending{Model: pricedTurns{usage: model.Usage{InputTokens: 1000, OutputTokens: 500}}}
	for range 2 {
		if _, err := m.Turn(context.Background(), model.Request{Price: price}, nil); err != nil {
			t.Fatal(err)
		}
	}
	// 1,000 input at $2/M and 500 output at $10/M: $0.007 a turn.
	if got := m.spent.Load(); got != 14_000 {
		t.Fatalf("spent %d micro-USD, want 14000", got)
	}
	var b strings.Builder
	tt := &test{}
	tt.cfg.transcript = &b
	tt.chatCost(m)
	if got, want := b.String(), "chat cost $0.0140: model turns $0.0140, judge $0.0000\n"; got != want {
		t.Fatalf("transcript %q, want %q", got, want)
	}
}
