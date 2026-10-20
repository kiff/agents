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
		MaxTokens: 8192, Effort: "high", Gateway: "https://gateway.test/mcp", KeyRef: "agents/" + id,
		Keep: keep, MaxCost: 500_000, MaxTurns: 20, History: 5,
	}
}

// Largest returns the largest definition Check accepts for tenant and id,
// its instructions made of what encoding makes longest: control characters,
// six bytes each once stored.
func Largest(tenant, id string) registry.Definition {
	d := Definition(tenant, id)
	d.Instructions = ""
	b, err := registry.Encode(d)
	if err != nil {
		panic(err)
	}
	d.Instructions = strings.Repeat("\x01", (registry.MaxSize-len(b))/6)
	return d
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

	t.Run("TheLargestDefinitionFits", func(t *testing.T) {
		s := newStore(t)
		want := Largest("t1", "payouts")
		if err := want.Check(); err != nil {
			t.Fatalf("Largest is refused: %v", err)
		}
		put(t, s, want)
		got, err := s.Get(ctx, "t1", "payouts")
		if err != nil {
			t.Fatal(err)
		}
		if got.Instructions != want.Instructions {
			t.Fatalf("got %d bytes of instructions back, want %d", len(got.Instructions), len(want.Instructions))
		}
	})

	// Instructions are full of '<', '>' and '&'; a store that escapes them
	// stores six bytes for each.
	t.Run("MarkupIsStoredAsIs", func(t *testing.T) {
		s := newStore(t)
		want := Largest("t1", "payouts")
		want.Instructions = strings.Repeat("<", len(want.Instructions)*5)
		if err := want.Check(); err != nil {
			t.Fatalf("refused: %v", err)
		}
		put(t, s, want)
		if got, err := s.Get(ctx, "t1", "payouts"); err != nil || got.Instructions != want.Instructions {
			t.Fatalf("got %d bytes of instructions back (err %v), want %d", len(got.Instructions), err, len(want.Instructions))
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
	versions(t, newStore)

}

// versions checks the agent's versions (kiff/tafs#107): kept apart from
// what's live until switched, every one listed, any one live again.
func versions(t *testing.T, newStore func(t *testing.T) registry.Agents) {
	ctx := context.Background()

	t.Run("APutVersionIsntLiveUntilSwitched", func(t *testing.T) {
		s := newStore(t)
		v1 := Definition("t1", "refunds")
		if err := s.Put(ctx, v1); err != nil {
			t.Fatal(err)
		}
		v2 := v1
		v2.Instructions = "Read the payment before you refund it."
		n, err := s.PutVersion(ctx, v2, registry.Version{Note: "reads first", By: "setup", Revisions: []string{"rev_2"}, Draft: "drf_2"})
		if err != nil || n != 2 {
			t.Fatalf("PutVersion = %d, %v; want 2 (the live one kept as 1)", n, err)
		}
		live, err := s.Get(ctx, "t1", "refunds")
		if err != nil || live.Instructions != v1.Instructions || live.Version != 1 {
			t.Fatalf("live after PutVersion = v%d %q, %v; want v1, unchanged", live.Version, live.Instructions, err)
		}
		vs, err := s.Versions(ctx, "t1", "refunds")
		if err != nil || len(vs) != 2 {
			t.Fatalf("Versions = %+v, %v", vs, err)
		}
		if !vs[0].Live || vs[1].Live || vs[1].N != 2 || vs[1].Note != "reads first" || vs[1].By != "setup" ||
			len(vs[1].Revisions) != 1 || vs[1].Revisions[0] != "rev_2" || vs[1].Draft != "drf_2" || vs[1].Created.IsZero() {
			t.Fatalf("Versions = %+v", vs)
		}
		if d1, _ := registry.Digest(v1); vs[0].Digest != d1 {
			t.Fatalf("v1's digest %q, want the live definition's %q", vs[0].Digest, d1)
		}
		got, err := s.GetVersion(ctx, "t1", "refunds", 2)
		if err != nil || got.Instructions != v2.Instructions || got.Version != 2 {
			t.Fatalf("GetVersion(2) = %+v, %v", got, err)
		}
	})

	t.Run("SwitchGoesForwardAndBack", func(t *testing.T) {
		s := newStore(t)
		v1 := Definition("t1", "refunds")
		if err := s.Put(ctx, v1); err != nil {
			t.Fatal(err)
		}
		v2 := v1
		v2.Instructions = "Version two."
		if _, err := s.PutVersion(ctx, v2, registry.Version{Note: "two"}); err != nil {
			t.Fatal(err)
		}
		for _, n := range []int{2, 1, 2} {
			if err := s.Switch(ctx, "t1", "refunds", n); err != nil {
				t.Fatalf("Switch(%d): %v", n, err)
			}
			live, _ := s.Get(ctx, "t1", "refunds")
			vs, _ := s.Versions(ctx, "t1", "refunds")
			if live.Version != n || !vs[n-1].Live || vs[2-n].Live {
				t.Fatalf("after Switch(%d): live v%d, versions %+v", n, live.Version, vs)
			}
		}
		if err := s.Switch(ctx, "t1", "refunds", 3); !errors.Is(err, registry.ErrNoVersion) {
			t.Fatalf("Switch(3) = %v, want ErrNoVersion", err)
		}
		if _, err := s.GetVersion(ctx, "t1", "refunds", 9); !errors.Is(err, registry.ErrNoVersion) {
			t.Fatalf("GetVersion(9) = %v, want ErrNoVersion", err)
		}
	})

	t.Run("PutOnAVersionedAgentIsItsNextVersionLive", func(t *testing.T) {
		s := newStore(t)
		d := Definition("t1", "refunds")
		if err := s.Put(ctx, d); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutVersion(ctx, d, registry.Version{Note: "same"}); err != nil {
			t.Fatal(err)
		}
		d.Instructions = "An upgrade."
		if err := s.Put(ctx, d); err != nil {
			t.Fatal(err)
		}
		live, _ := s.Get(ctx, "t1", "refunds")
		vs, _ := s.Versions(ctx, "t1", "refunds")
		if live.Version != 3 || live.Instructions != "An upgrade." || len(vs) != 3 || !vs[2].Live {
			t.Fatalf("live v%d %q, versions %+v; want the upgrade as v3, live", live.Version, live.Instructions, vs)
		}
	})

	t.Run("VersionsAreTheirAgentsAndTenants", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.PutVersion(ctx, Definition("t1", "nobody"), registry.Version{}); !errors.Is(err, registry.ErrNotFound) {
			t.Fatalf("PutVersion for an agent not stored = %v, want ErrNotFound", err)
		}
		d := Definition("t1", "refunds")
		if err := s.Put(ctx, d); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutVersion(ctx, d, registry.Version{}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetVersion(ctx, "t2", "refunds", 1); !errors.Is(err, registry.ErrNoVersion) {
			t.Fatalf("another tenant's version = %v, want ErrNoVersion", err)
		}
		if err := s.Switch(ctx, "t2", "refunds", 1); !errors.Is(err, registry.ErrNotFound) {
			t.Fatalf("Switch in another tenant = %v, want ErrNotFound", err)
		}
		if err := s.Delete(ctx, "t1", "refunds"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetVersion(ctx, "t1", "refunds", 1); !errors.Is(err, registry.ErrNoVersion) {
			t.Fatalf("a version after Delete = %v, want ErrNoVersion", err)
		}
	})
}
