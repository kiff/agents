// Package usagetest checks a usage.Store against what the package promises,
// so the memory and DynamoDB stores are held to the same rules.
package usagetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/usage"
)

// Run checks the store newStore makes, one fresh store per test.
func Run(t *testing.T, newStore func(*testing.T) usage.Store) {
	ctx := context.Background()
	until := time.Now().Add(40 * 24 * time.Hour)

	t.Run("a period nobody used is zero", func(t *testing.T) {
		u := newStore(t)
		p, err := u.Get(ctx, "t1", "2026-10-20")
		if err != nil || p != (usage.Period{}) {
			t.Fatalf("got %+v, %v", p, err)
		}
	})

	t.Run("adds sum, per tenant and per period", func(t *testing.T) {
		u := newStore(t)
		for _, a := range []struct {
			tenant, period string
			c              int64
		}{{"t1", "p1", 1200}, {"t1", "p1", 300}, {"t1", "p2", 7}, {"t2", "p1", 50}} {
			if err := u.Add(ctx, a.tenant, a.period, a.c, until); err != nil {
				t.Fatal(err)
			}
		}
		for _, w := range []struct {
			tenant, period string
			spent          int64
		}{{"t1", "p1", 1500}, {"t1", "p2", 7}, {"t2", "p1", 50}, {"t2", "p2", 0}} {
			if p, _ := u.Get(ctx, w.tenant, w.period); p.Spent != w.spent {
				t.Errorf("%s %s: spent %d, want %d", w.tenant, w.period, p.Spent, w.spent)
			}
		}
	})

	t.Run("concurrent adds lose nothing", func(t *testing.T) {
		u := newStore(t)
		var wg sync.WaitGroup
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := u.Add(ctx, "t1", "p1", 10, until); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if p, _ := u.Get(ctx, "t1", "p1"); p.Spent != 200 {
			t.Fatalf("spent %d, want 200", p.Spent)
		}
	})

	t.Run("erasing a tenant leaves others", func(t *testing.T) {
		u := newStore(t)
		for _, tenant := range []string{"t1", "t2"} {
			for _, period := range []string{"p1", "p2"} {
				if err := u.Add(ctx, tenant, period, 5, until); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := u.EraseTenant(ctx, "t1"); err != nil {
			t.Fatal(err)
		}
		for _, period := range []string{"p1", "p2"} {
			if p, _ := u.Get(ctx, "t1", period); p != (usage.Period{}) {
				t.Errorf("t1 %s kept: %+v", period, p)
			}
			if p, _ := u.Get(ctx, "t2", period); p.Spent != 5 {
				t.Errorf("t2 %s: %+v", period, p)
			}
		}
	})
}
