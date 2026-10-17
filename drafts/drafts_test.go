package drafts_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/kiff/agents/drafts"
	"github.com/kiff/agents/store/memory"
)

var ctx = context.Background()

// kiff is KIFF as a draft sees it: drafts it accepted, which the owner may
// confirm, each claimable once with its secret.
type kiff struct {
	confirmed map[string]bool
	claimed   map[string]bool
	claims    int
	keys      map[string]drafts.Me // key → who it speaks for
	mint      drafts.Me            // who the next minted key speaks for
}

func newKIFF() *kiff {
	return &kiff{confirmed: map[string]bool{}, claimed: map[string]bool{}, keys: map[string]drafts.Me{},
		mint: drafts.Me{TenantID: "t1", AgentID: "agt_refunds"}}
}

func (k *kiff) Submit(_ context.Context, key string, body json.RawMessage) (drafts.Draft, string, error) {
	var b struct {
		AgentID string `json:"agent_id"`
	}
	_ = json.Unmarshal(body, &b)
	return drafts.Draft{ID: "drf_1", AgentID: b.AgentID, Status: "draft"}, "kiff_claim_s3cret", nil
}

func (k *kiff) Claim(_ context.Context, draft, secret string) (drafts.Claimed, error) {
	if secret != "kiff_claim_s3cret" || !k.confirmed[draft] || k.claimed[draft] {
		return drafts.Claimed{}, drafts.ErrNotClaimable
	}
	k.claimed[draft] = true
	k.claims++
	k.keys["kiff_key_taf"] = k.mint
	return drafts.Claimed{AgentID: "agt_refunds", Key: "kiff_key_taf", KeyID: "k_taf"}, nil
}

func (k *kiff) Me(_ context.Context, key string) (drafts.Me, error) {
	me, ok := k.keys[key]
	if !ok {
		return drafts.Me{}, errors.New("401")
	}
	return me, nil
}

func newService() (*drafts.Service, *kiff, *memory.Drafts) {
	k, m := newKIFF(), memory.NewDrafts()
	return &drafts.Service{Store: m, Keys: m, KIFF: k}, k, m
}

func submit(t *testing.T, s *drafts.Service) drafts.Draft {
	t.Helper()
	d, err := s.Submit(ctx, "t1", "kiff_key_setup", json.RawMessage(`{"agent_id":"agt_refunds","label":"Refunds","cards":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// What the submit returns, which is what the model reads, has no claim
// secret; the secret is kept under the tenant.
func TestSubmitKeepsTheSecretFromTheModel(t *testing.T) {
	s, _, m := newService()
	d := submit(t, s)
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), "kiff_claim_") || d.ID != "drf_1" {
		t.Fatalf("submit returned %s", b)
	}
	r, err := m.Get(ctx, "t1", "drf_1")
	if err != nil || r.Secret != "kiff_claim_s3cret" || r.AgentID != "agt_refunds" {
		t.Fatalf("kept %+v, %v", r, err)
	}
}

// Before the owner confirms, KIFF gives nothing and the secret stays; after,
// the key is stored under the reference, the secret is gone, and a retry
// answers the same without claiming again.
func TestClaim(t *testing.T) {
	s, k, m := newService()
	submit(t, s)
	if _, err := s.Claim(ctx, "t1", "drf_1", "kiff/taf-1"); !errors.Is(err, drafts.ErrNotClaimable) {
		t.Fatalf("before confirmation: %v", err)
	}
	if r, _ := m.Get(ctx, "t1", "drf_1"); r.Secret == "" {
		t.Fatal("a refused claim dropped the secret")
	}
	k.confirmed["drf_1"] = true
	agent, err := s.Claim(ctx, "t1", "drf_1", "kiff/taf-1")
	if err != nil || agent != "agt_refunds" {
		t.Fatalf("claim: %q %v", agent, err)
	}
	if key, ok, _ := m.Key(ctx, "t1", "kiff/taf-1"); !ok || key != "kiff_key_taf" {
		t.Fatalf("stored %q %v", key, ok)
	}
	if r, _ := m.Get(ctx, "t1", "drf_1"); r.Secret != "" || !r.Claimed || r.KeyRef != "kiff/taf-1" {
		t.Fatalf("record after the claim %+v", r)
	}
	if agent, err := s.Claim(ctx, "t1", "drf_1", "kiff/taf-1"); err != nil || agent != "agt_refunds" || k.claims != 1 {
		t.Fatalf("retry: %q %v, claims %d", agent, err, k.claims)
	}
	if _, err := s.Claim(ctx, "t1", "drf_1", "kiff/taf-2"); !errors.Is(err, drafts.ErrOtherRef) {
		t.Fatalf("another ref: %v", err)
	}
	if _, err := s.Claim(ctx, "t2", "drf_1", "kiff/taf-1"); !errors.Is(err, drafts.ErrNotFound) {
		t.Fatalf("another tenant: %v", err)
	}
}

// Another agent's key under the reference stays, and KIFF isn't asked to
// mint a key that would have nowhere to go.
func TestClaimNeverReplacesAnotherKey(t *testing.T) {
	s, k, m := newService()
	submit(t, s)
	k.confirmed["drf_1"] = true
	_ = m.Create(ctx, "t1", "kiff/taf-1", "kiff_key_other")
	k.keys["kiff_key_other"] = drafts.Me{TenantID: "t1", AgentID: "agt_other"}
	if _, err := s.Claim(ctx, "t1", "drf_1", "kiff/taf-1"); !errors.Is(err, drafts.ErrTaken) {
		t.Fatalf("got %v, want ErrTaken", err)
	}
	if key, _, _ := m.Key(ctx, "t1", "kiff/taf-1"); key != "kiff_key_other" || k.claims != 0 {
		t.Fatalf("the other key was touched (%q), or KIFF claimed (%d)", key, k.claims)
	}
}

// A claim that stored the key and stopped before recording itself is
// finished by the retry, without a second claim.
func TestARetryFinishesAnInterruptedClaim(t *testing.T) {
	s, k, m := newService()
	submit(t, s)
	k.keys["kiff_key_taf"] = drafts.Me{TenantID: "t1", AgentID: "agt_refunds"}
	_ = m.Create(ctx, "t1", "kiff/taf-1", "kiff_key_taf")
	agent, err := s.Claim(ctx, "t1", "drf_1", "kiff/taf-1")
	if err != nil || agent != "agt_refunds" || k.claims != 0 {
		t.Fatalf("recovery: %q %v, claims %d", agent, err, k.claims)
	}
	if r, _ := m.Get(ctx, "t1", "drf_1"); !r.Claimed || r.Secret != "" {
		t.Fatalf("record %+v", r)
	}
}

// A key KIFF minted for another tenant or agent is never stored.
func TestAKeyForAnotherAgentIsNotStored(t *testing.T) {
	for _, mint := range []drafts.Me{{TenantID: "t2", AgentID: "agt_refunds"}, {TenantID: "t1", AgentID: "agt_other"}, {TenantID: "t1"}} {
		s, k, m := newService()
		submit(t, s)
		k.confirmed["drf_1"] = true
		k.mint = mint
		if _, err := s.Claim(ctx, "t1", "drf_1", "kiff/taf-1"); !errors.Is(err, drafts.ErrMismatch) {
			t.Fatalf("%+v: %v", mint, err)
		}
		if _, ok, _ := m.Key(ctx, "t1", "kiff/taf-1"); ok {
			t.Fatalf("%+v: the key was stored", mint)
		}
		if r, _ := m.Get(ctx, "t1", "drf_1"); r.Secret != "" {
			t.Fatalf("%+v: the used secret was kept", mint)
		}
	}
}

// flaky fails the first Create, as a throttled Secrets Manager might.
type flaky struct {
	*memory.Drafts
	failed bool
}

func (f *flaky) Create(ctx context.Context, tenant, ref, key string) error {
	if !f.failed {
		f.failed = true
		return errors.New("throttled")
	}
	return f.Drafts.Create(ctx, tenant, ref, key)
}

// A key KIFF minted is held in the draft's record until it is stored, so a
// claim that fails after the mint is finished by its retry: KIFF mints a
// key only once.
func TestARetryFinishesAfterTheKeyWasMinted(t *testing.T) {
	k, m := newKIFF(), memory.NewDrafts()
	s := &drafts.Service{Store: m, Keys: &flaky{Drafts: m}, KIFF: k}
	submit(t, s)
	k.confirmed["drf_1"] = true
	if _, err := s.Claim(ctx, "t1", "drf_1", "kiff/taf-1"); err == nil {
		t.Fatal("the first claim should have failed to store the key")
	}
	if r, _ := m.Get(ctx, "t1", "drf_1"); r.Key != "kiff_key_taf" || r.Secret != "" {
		t.Fatalf("after the failed store the record is %+v", r)
	}
	agent, err := s.Claim(ctx, "t1", "drf_1", "kiff/taf-1")
	if err != nil || agent != "agt_refunds" || k.claims != 1 {
		t.Fatalf("retry: %q %v, claims %d", agent, err, k.claims)
	}
	if key, ok, _ := m.Key(ctx, "t1", "kiff/taf-1"); !ok || key != "kiff_key_taf" {
		t.Fatalf("stored %q %v", key, ok)
	}
	if r, _ := m.Get(ctx, "t1", "drf_1"); r.Key != "" || !r.Claimed {
		t.Fatalf("the record still holds the key, or isn't claimed: %+v", r)
	}
}

// A key that isn't the draft's agent's is kept in the record, flagged, and
// logged with what the owner needs to revoke it; it is never stored where a
// definition can name it, and the claim isn't tried again.
func TestAMismatchedKeyIsKeptForRevoking(t *testing.T) {
	s, k, m := newService()
	var logs bytes.Buffer
	s.Log = slog.New(slog.NewTextHandler(&logs, nil))
	submit(t, s)
	k.confirmed["drf_1"] = true
	k.mint = drafts.Me{TenantID: "t1", AgentID: "agt_other"}
	for range 2 {
		if _, err := s.Claim(ctx, "t1", "drf_1", "kiff/taf-1"); !errors.Is(err, drafts.ErrMismatch) {
			t.Fatalf("got %v, want ErrMismatch", err)
		}
	}
	if k.claims != 1 {
		t.Fatalf("claimed %d times", k.claims)
	}
	r, _ := m.Get(ctx, "t1", "drf_1")
	if !r.Mismatch || r.Key == "" || r.KeyID != "k_taf" {
		t.Fatalf("record %+v", r)
	}
	if !strings.Contains(logs.String(), "drf_1") || !strings.Contains(logs.String(), "k_taf") {
		t.Fatalf("log %q", logs.String())
	}
}
