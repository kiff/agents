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
	switching(t, newStore)
	cards(t, newStore)
	trials(t, newStore)

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
		if !vs[0].Live || vs[0].Note != registry.NoteFirst || vs[1].Live || vs[1].N != 2 || vs[1].Note != "reads first" || vs[1].By != "setup" ||
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
		if live.Version != 3 || live.Instructions != "An upgrade." || len(vs) != 3 || !vs[2].Live || vs[2].Note != registry.NotePut {
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

// switching checks the mark an agent carries while it switches versions
// (kiff/tafs#107).
func switching(t *testing.T, newStore func(t *testing.T) registry.Agents) {
	ctx := context.Background()
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

	t.Run("OneSwitchAtATime", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.BeginSwitch(ctx, "t1", "refunds", registry.Switching{N: 2, At: at}); !errors.Is(err, registry.ErrNotFound) {
			t.Fatalf("BeginSwitch on no agent = %v; want ErrNotFound", err)
		}
		if err := s.Put(ctx, Definition("t1", "refunds")); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := s.Switching(ctx, "t1", "refunds"); ok || err != nil {
			t.Fatalf("Switching before any = %v, %v; want none", ok, err)
		}
		got, err := s.BeginSwitch(ctx, "t1", "refunds", registry.Switching{N: 2, Draft: "drf_2", At: at})
		if err != nil || got.N != 2 || got.Draft != "drf_2" || !got.At.Equal(at) {
			t.Fatalf("BeginSwitch = %+v, %v", got, err)
		}
		again, err := s.BeginSwitch(ctx, "t1", "refunds", registry.Switching{N: 2, Draft: "drf_x", At: at.Add(time.Hour)})
		if err != nil || again.Draft != "drf_2" || !again.At.Equal(at) {
			t.Fatalf("BeginSwitch to the same version again = %+v, %v; want the first mark kept", again, err)
		}
		other, err := s.BeginSwitch(ctx, "t1", "refunds", registry.Switching{N: 3, At: at})
		if !errors.Is(err, registry.ErrSwitching) || other.N != 2 {
			t.Fatalf("BeginSwitch to another version = %+v, %v; want ErrSwitching with the mark", other, err)
		}
		if sw, ok, err := s.Switching(ctx, "t1", "refunds"); !ok || err != nil || sw.N != 2 {
			t.Fatalf("Switching = %+v, %v, %v", sw, ok, err)
		}
		// Its draft replaced in place: the same mark, begun when it was.
		if _, err := s.SwitchDraft(ctx, "t1", "refunds", 3, "drf_3"); !errors.Is(err, registry.ErrNoSwitch) {
			t.Fatalf("SwitchDraft for another version = %v; want ErrNoSwitch", err)
		}
		if _, err := s.SwitchDraft(ctx, "t2", "refunds", 2, "drf_3"); !errors.Is(err, registry.ErrNoSwitch) {
			t.Fatalf("SwitchDraft in another tenant = %v; want ErrNoSwitch", err)
		}
		if sw, err := s.SwitchDraft(ctx, "t1", "refunds", 2, "drf_2b"); err != nil || sw.N != 2 || sw.Draft != "drf_2b" || !sw.At.Equal(at) {
			t.Fatalf("SwitchDraft = %+v, %v; want version 2 waiting for drf_2b, begun at %v", sw, err, at)
		}
		if sw, ok, err := s.Switching(ctx, "t1", "refunds"); !ok || err != nil || sw.Draft != "drf_2b" || !sw.At.Equal(at) {
			t.Fatalf("Switching after SwitchDraft = %+v, %v, %v", sw, ok, err)
		}
		if _, ok, _ := s.Switching(ctx, "t2", "refunds"); ok {
			t.Fatal("another tenant's agent of the same id is switching")
		}
		if err := s.EndSwitch(ctx, "t1", "refunds"); err != nil {
			t.Fatal(err)
		}
		if err := s.EndSwitch(ctx, "t1", "refunds"); err != nil {
			t.Fatalf("EndSwitch with none = %v; want nil", err)
		}
		if _, err := s.SwitchDraft(ctx, "t1", "refunds", 2, "drf_2c"); !errors.Is(err, registry.ErrNoSwitch) {
			t.Fatalf("SwitchDraft after the end = %v; want ErrNoSwitch", err)
		}
		if _, err := s.BeginSwitch(ctx, "t1", "refunds", registry.Switching{N: 3, At: at}); err != nil {
			t.Fatalf("BeginSwitch after the end = %v", err)
		}
	})

	t.Run("DeleteEndsTheSwitch", func(t *testing.T) {
		s := newStore(t)
		if err := s.Put(ctx, Definition("t1", "refunds")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.BeginSwitch(ctx, "t1", "refunds", registry.Switching{N: 2, At: at}); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, "t1", "refunds"); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := s.Switching(ctx, "t1", "refunds"); ok || err != nil {
			t.Fatalf("Switching after Delete = %v, %v; want none", ok, err)
		}
	})
}

// cards checks that a version records the Cards it runs under
// (kiff/tafs#107).
func cards(t *testing.T, newStore func(t *testing.T) registry.Agents) {
	ctx := context.Background()
	t.Run("AVersionRecordsItsCards", func(t *testing.T) {
		s := newStore(t)
		if err := s.Put(ctx, Definition("t1", "refunds")); err != nil {
			t.Fatal(err)
		}
		v2 := Definition("t1", "refunds")
		v2.Instructions = "Version two."
		if _, err := s.PutVersion(ctx, v2, registry.Version{Note: "two"}); err != nil {
			t.Fatal(err)
		}
		want := []registry.CardRevision{{ID: "card_refund", Revision: 3}, {ID: "card_read", Revision: 1}}
		if err := s.SetCards(ctx, "t1", "refunds", 2, want); err != nil {
			t.Fatal(err)
		}
		vs, err := s.Versions(ctx, "t1", "refunds")
		if err != nil || len(vs) != 2 || !reflect.DeepEqual(vs[1].Cards, want) || len(vs[0].Cards) != 0 || vs[1].Note != "two" {
			t.Fatalf("Versions = %+v, %v", vs, err)
		}
		if d, err := s.GetVersion(ctx, "t1", "refunds", 2); err != nil || d.Instructions != "Version two." {
			t.Fatalf("the definition changed with its Cards: %q, %v", d.Instructions, err)
		}
		if err := s.SetCards(ctx, "t1", "refunds", 9, want); !errors.Is(err, registry.ErrNoVersion) {
			t.Fatalf("SetCards on no version = %v", err)
		}
	})
}

// trials checks an agent's open trial (kiff/tafs#173): one per agent,
// replaced whole, listed across tenants for the sweep, ended only by its
// own ID, and summed up on its version once ended.
func trials(t *testing.T, newStore func(t *testing.T) registry.Agents) {
	ctx := context.Background()
	at := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

	t.Run("OneOpenTrialPerAgent", func(t *testing.T) {
		s := newStore(t)
		first := registry.Trial{Tenant: "t1", Agent: "refunds", ID: "tr_1", N: 2, Draft: "drf_2", Started: at, Used: at}
		if err := s.PutTrial(ctx, first); !errors.Is(err, registry.ErrNotFound) {
			t.Fatalf("PutTrial on no agent = %v; want ErrNotFound", err)
		}
		for _, k := range [][2]string{{"t1", "refunds"}, {"t2", "refunds"}} {
			if err := s.Put(ctx, Definition(k[0], k[1])); err != nil {
				t.Fatal(err)
			}
		}
		if _, ok, err := s.Trial(ctx, "t1", "refunds"); ok || err != nil {
			t.Fatalf("Trial before any = %v, %v; want none", ok, err)
		}
		if err := s.PutTrial(ctx, first); err != nil {
			t.Fatal(err)
		}
		if got, ok, err := s.Trial(ctx, "t1", "refunds"); !ok || err != nil || !reflect.DeepEqual(got, first) {
			t.Fatalf("Trial = %+v, %v, %v; want %+v", got, ok, err, first)
		}
		if _, ok, _ := s.Trial(ctx, "t2", "refunds"); ok {
			t.Fatal("another tenant's agent of the same id has the trial")
		}
		// Its first message names its session; a later one, when it was used.
		used := first
		used.Session, used.Used = "sess_1", at.Add(time.Hour)
		if err := s.PutTrial(ctx, used); err != nil {
			t.Fatal(err)
		}
		other := registry.Trial{Tenant: "t2", Agent: "refunds", ID: "tr_9", N: 3, Draft: "drf_9", Started: at, Used: at}
		if err := s.PutTrial(ctx, other); err != nil {
			t.Fatal(err)
		}
		all, err := s.Trials(ctx)
		if err != nil || len(all) != 2 || !reflect.DeepEqual(all[0], used) || !reflect.DeepEqual(all[1], other) {
			t.Fatalf("Trials = %+v, %v", all, err)
		}
		// A trial that replaced it isn't ended by the old one's ID.
		second := registry.Trial{Tenant: "t1", Agent: "refunds", ID: "tr_2", N: 3, Draft: "drf_3", Started: at, Used: at}
		if err := s.PutTrial(ctx, second); err != nil {
			t.Fatal(err)
		}
		if err := s.EndTrial(ctx, "t1", "refunds", "tr_1"); err != nil {
			t.Fatalf("EndTrial of a replaced one = %v; want nil", err)
		}
		if got, ok, _ := s.Trial(ctx, "t1", "refunds"); !ok || got.ID != "tr_2" {
			t.Fatalf("after ending the replaced one, Trial = %+v, %v; want tr_2 open", got, ok)
		}
		if err := s.EndTrial(ctx, "t1", "refunds", "tr_2"); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := s.Trial(ctx, "t1", "refunds"); ok || err != nil {
			t.Fatalf("Trial after EndTrial = %v, %v; want none", ok, err)
		}
		if err := s.EndTrial(ctx, "t1", "refunds", "tr_2"); err != nil {
			t.Fatalf("EndTrial with none = %v; want nil", err)
		}
		if err := s.Delete(ctx, "t2", "refunds"); err != nil {
			t.Fatal(err)
		}
		if all, err := s.Trials(ctx); err != nil || len(all) != 0 {
			t.Fatalf("Trials after Delete = %+v, %v; want none", all, err)
		}
	})

	t.Run("AVersionKeepsItsEndedTrials", func(t *testing.T) {
		s := newStore(t)
		if err := s.Put(ctx, Definition("t1", "refunds")); err != nil {
			t.Fatal(err)
		}
		v2 := Definition("t1", "refunds")
		v2.Instructions = "Version two."
		if _, err := s.PutVersion(ctx, v2, registry.Version{Note: "two"}); err != nil {
			t.Fatal(err)
		}
		want := []registry.TrialSummary{
			{Started: at, Ended: at.Add(time.Hour), Why: registry.TrialBack, Calls: 3, Held: 1},
			{Started: at.Add(2 * time.Hour), Ended: at.Add(3 * time.Hour), Why: registry.TrialInstalled, Calls: 2, Refused: 1},
		}
		for _, sum := range want {
			if err := s.AddTrialSummary(ctx, "t1", "refunds", 2, sum); err != nil {
				t.Fatal(err)
			}
		}
		vs, err := s.Versions(ctx, "t1", "refunds")
		if err != nil || len(vs) != 2 || !reflect.DeepEqual(vs[1].Trials, want) || len(vs[0].Trials) != 0 || vs[1].Note != "two" {
			t.Fatalf("Versions = %+v, %v", vs, err)
		}
		if err := s.AddTrialSummary(ctx, "t1", "refunds", 9, want[0]); !errors.Is(err, registry.ErrNoVersion) {
			t.Fatalf("AddTrialSummary on no version = %v", err)
		}
	})
}
