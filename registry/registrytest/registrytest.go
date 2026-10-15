// Package registrytest holds the checks every registry.Agents store must pass.
// Each store's tests call Run with a function that returns an empty store.
package registrytest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/session"
)

// Definition returns a valid definition for tenant and id.
func Definition(tenant, id string) registry.Definition {
	keep := session.DefaultPolicy(30 * 24 * time.Hour)
	keep[session.Usage] = session.Rule{Keep: session.Kept, Retention: 90 * 24 * time.Hour}
	return registry.Definition{
		Tenant: tenant, ID: id,
		Instructions: "You reconcile payouts.\n\nKIFF's skill: every write goes through KIFF.",
		Model:        "a-model", Price: model.Price{Input: 3_000_000, Output: 15_000_000, CacheRead: 300_000, CacheWrite: 3_750_000},
		MaxTokens: 8192, Gateway: "https://gateway.test/mcp", KeyRef: "agents/" + id,
		Keep: keep, MaxCost: 500_000, MaxTurns: 20, History: 5,
	}
}

// Run runs every check against the stores newStore returns, one per check.
func Run(t *testing.T, newStore func(t *testing.T) registry.Agents) {
	ctx := context.Background()
	put := func(t *testing.T, s registry.Agents, d registry.Definition) {
		t.Helper()
		if err := s.Put(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("GetReturnsWhatWasPut", func(t *testing.T) {
		s := newStore(t)
		want := Definition("t1", "payouts")
		put(t, s, want)
		got, err := s.Get(ctx, "t1", "payouts")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v\nwant %+v", got, want)
		}
	})

	t.Run("PutReplaces", func(t *testing.T) {
		s := newStore(t)
		d := Definition("t1", "payouts")
		put(t, s, d)
		d.Instructions, d.MaxCost = "New instructions.", 1_000_000
		d.Keep = session.DefaultPolicy(time.Hour)
		put(t, s, d)
		got, err := s.Get(ctx, "t1", "payouts")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, d) {
			t.Fatalf("got %+v\nwant the replacement %+v", got, d)
		}
	})

	t.Run("MissingIsNotFound", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.Get(ctx, "t1", "nobody"); !errors.Is(err, registry.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("TenantsAreApart", func(t *testing.T) {
		s := newStore(t)
		mine, theirs := Definition("t1", "payouts"), Definition("t2", "payouts")
		theirs.Instructions = "Another tenant's agent."
		put(t, s, mine)
		put(t, s, theirs)
		if got, err := s.Get(ctx, "t1", "payouts"); err != nil || got.Instructions != mine.Instructions {
			t.Fatalf("t1 got %q (err %v), want its own agent", got.Instructions, err)
		}
		if _, err := s.Get(ctx, "t3", "payouts"); !errors.Is(err, registry.ErrNotFound) {
			t.Fatalf("a tenant with no agents found one: %v", err)
		}
		if err := s.Delete(ctx, "t2", "payouts"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(ctx, "t1", "payouts"); err != nil {
			t.Fatalf("deleting t2's agent removed t1's: %v", err)
		}
		if list, err := s.List(ctx, "t2"); err != nil || len(list) != 0 {
			t.Fatalf("t2 lists %d agents (err %v) after deleting its only one", len(list), err)
		}
	})

	t.Run("ListIsTheTenantsByID", func(t *testing.T) {
		s := newStore(t)
		// Enough agents that a store returning them unsorted is caught.
		for _, id := range []string{"refunds", "chargebacks", "payouts", "e-invoices", "disputes", "tax", "billing", "vendors"} {
			put(t, s, Definition("t1", id))
		}
		put(t, s, Definition("t2", "other"))
		list, err := s.List(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, d := range list {
			if d.Tenant != "t1" {
				t.Fatalf("t1's list holds an agent of %q", d.Tenant)
			}
			ids = append(ids, d.ID)
		}
		const want = "billing,chargebacks,disputes,e-invoices,payouts,refunds,tax,vendors"
		if got := strings.Join(ids, ","); got != want {
			t.Fatalf("listed %s, want %s", got, want)
		}
		if list[0].Keep == nil || list[0].Price != Definition("t1", "x").Price {
			t.Fatalf("a listed definition is incomplete: %+v", list[0])
		}
	})

	t.Run("DeleteIsSafeToRepeat", func(t *testing.T) {
		s := newStore(t)
		put(t, s, Definition("t1", "payouts"))
		for i := range 2 {
			if err := s.Delete(ctx, "t1", "payouts"); err != nil {
				t.Fatalf("delete %d: %v", i+1, err)
			}
		}
		if _, err := s.Get(ctx, "t1", "payouts"); !errors.Is(err, registry.ErrNotFound) {
			t.Fatalf("got %v after delete, want ErrNotFound", err)
		}
	})

	t.Run("StoredIsACopy", func(t *testing.T) {
		s := newStore(t)
		d := Definition("t1", "payouts")
		put(t, s, d)
		d.Keep[session.ToolResult] = session.Rule{Keep: session.Kept}
		got, _ := s.Get(ctx, "t1", "payouts")
		got.Keep[session.ToolResult] = session.Rule{Keep: session.Kept}
		again, err := s.Get(ctx, "t1", "payouts")
		if err != nil {
			t.Fatal(err)
		}
		if again.Keep[session.ToolResult].Keep == session.Kept {
			t.Fatal("changing a definition after Put, or one Get returned, changed the stored keep rules")
		}
	})
}
