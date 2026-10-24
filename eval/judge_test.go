package eval_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kiff/agents/eval"
	"github.com/kiff/agents/model"
)

// votes is a judge model that fails the first `fails` questions it's asked, then passes.
type votes struct {
	fails int32
	asked atomic.Int32
	err   error
}

func (v *votes) Turn(context.Context, model.Request, func(string)) (model.Reply, error) {
	n := v.asked.Add(1)
	if v.err != nil {
		return model.Reply{}, v.err
	}
	pass := "true"
	if n <= v.fails {
		pass = "false"
	}
	return model.Reply{Blocks: []model.Block{{Kind: model.Text, Text: `{"pass": ` + pass + `, "why": "too long"}`}}}, nil
}

// One judge's dissent is a warning, not a fail; most judges failing it fails it (#209).
func TestAskMajority(t *testing.T) {
	for _, c := range []struct {
		fails int32
		pass  bool
		split int
	}{{0, true, 0}, {1, true, 1}, {2, false, 0}, {3, false, 0}} {
		v := &votes{fails: c.fails}
		g, err := (&eval.Judge{Model: v, ModelID: "judge"}).AskMajority(context.Background(), "concise", "text")
		if err != nil {
			t.Fatal(err)
		}
		if g.Pass != c.pass || g.Split != c.split || v.asked.Load() != eval.Votes {
			t.Errorf("%d fails: pass %v split %d asked %d, want pass %v split %d asked %d", c.fails, g.Pass, g.Split, v.asked.Load(), c.pass, c.split, eval.Votes)
		}
		if c.fails > 0 && g.Why != "too long" {
			t.Errorf("%d fails: why %q, want a failing vote's", c.fails, g.Why)
		}
	}
}

func TestAskMajorityFailsOnAnError(t *testing.T) {
	v := &votes{err: errors.New("overloaded")}
	if _, err := (&eval.Judge{Model: v, ModelID: "judge"}).AskMajority(context.Background(), "concise", "text"); err == nil || !strings.Contains(err.Error(), "overloaded") {
		t.Fatalf("err %v", err)
	}
}
