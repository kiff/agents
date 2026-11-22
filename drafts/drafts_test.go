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
	meFails   int                  // how many /v1/me calls fail before answering
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
	// An agent that holds Cards gets a revision draft: no key, no claim secret.
	if b.AgentID == "agt_holds_cards" {
		return drafts.Draft{ID: "drf_2", AgentID: b.AgentID, Status: "draft", Revision: true}, "", nil
	}
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
	if k.meFails > 0 {
		k.meFails--
		return drafts.Me{}, errors.New("503: KIFF is busy")
	}
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
	d, err := s.Submit(ctx, "t1", "kiff_key_setup", json.RawMessage(`{"agent_id":"agt_refunds","label":"Refunds","cards":[]}`), drafts.Submitted{})
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

// A revision draft (kiff/tafs#107) revises the Cards of an agent that has its
// key already: KIFF gives no claim secret, and none is kept or needed.
func TestSubmitTakesARevisionWithNoClaimSecret(t *testing.T) {
	s, _, m := newService()
	d, err := s.Submit(ctx, "t1", "kiff_key_setup", json.RawMessage(`{"agent_id":"agt_holds_cards","label":"Refunds","cards":[]}`), drafts.Submitted{})
	if err != nil || d.ID != "drf_2" || !d.Revision {
		t.Fatalf("submit = %+v, %v", d, err)
	}
	if _, err := m.Get(ctx, "t1", "drf_2"); err == nil {
		t.Fatal("a record was kept for a revision draft, which has nothing to claim")
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
	// The record as stored holds no credential after the claim: neither the
	// claim secret nor the key, only where the key went.
	if r, _ := m.Get(ctx, "t1", "drf_1"); r != (drafts.Record{AgentID: "agt_refunds", KeyRef: "kiff/taf-1", Claimed: true}) {
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
	if r, _ := m.Get(ctx, "t1", "drf_1"); r != (drafts.Record{AgentID: "agt_refunds", KeyRef: "kiff/taf-1", Claimed: true}) {
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
	if r, _ := m.Get(ctx, "t1", "drf_1"); r != (drafts.Record{AgentID: "agt_refunds", KeyRef: "kiff/taf-1", Claimed: true}) {
		t.Fatalf("the record still holds a credential, or isn't claimed: %+v", r)
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

// KIFF failing to say who a key is, right after the claim, is not a
// mismatch: the key stays held, and the retry asks again and finishes.
func TestKIFFFailingAfterTheClaimIsNotAMismatch(t *testing.T) {
	s, k, m := newService()
	submit(t, s)
	k.confirmed["drf_1"] = true
	k.meFails = 1
	if _, err := s.Claim(ctx, "t1", "drf_1", "kiff/taf-1"); err == nil || errors.Is(err, drafts.ErrMismatch) {
		t.Fatalf("got %v, want KIFF's error", err)
	}
	if r, _ := m.Get(ctx, "t1", "drf_1"); r.Mismatch || r.Key != "kiff_key_taf" {
		t.Fatalf("after the failed /v1/me the record is %+v", r)
	}
	agent, err := s.Claim(ctx, "t1", "drf_1", "kiff/taf-1")
	if err != nil || agent != "agt_refunds" || k.claims != 1 {
		t.Fatalf("retry: %q %v, claims %d", agent, err, k.claims)
	}
	if key, ok, _ := m.Key(ctx, "t1", "kiff/taf-1"); !ok || key != "kiff_key_taf" {
		t.Fatalf("stored %q %v", key, ok)
	}
}

// KIFF failing to say whose key is already under the reference is an
// error to retry, not a reason to refuse the reference.
func TestKIFFFailingOnAHeldRefIsNotTaken(t *testing.T) {
	s, k, m := newService()
	submit(t, s)
	_ = m.Create(ctx, "t1", "kiff/taf-1", "kiff_key_taf")
	k.keys["kiff_key_taf"] = drafts.Me{TenantID: "t1", AgentID: "agt_refunds"}
	k.meFails = 1
	if _, err := s.Claim(ctx, "t1", "drf_1", "kiff/taf-1"); err == nil || errors.Is(err, drafts.ErrTaken) {
		t.Fatalf("got %v, want KIFF's error", err)
	}
	if agent, err := s.Claim(ctx, "t1", "drf_1", "kiff/taf-1"); err != nil || agent != "agt_refunds" {
		t.Fatalf("retry: %q %v", agent, err)
	}
}

// The drafts no taf has claimed are listed with their name and who the
// setup ran for, from the tenant's own records only, never their secret:
// a claimed draft or one held back as a mismatch isn't listed.
func TestUnclaimedListsTheDraftsNoTafHasClaimed(t *testing.T) {
	s, _, m := newService()
	owner := &drafts.Requester{Kind: "member", ID: "user_owner"}
	if _, err := s.Submit(ctx, "t1", "kiff_key_setup", json.RawMessage(`{"agent_id":"agt_refunds","label":"Refunds","cards":[]}`), drafts.Submitted{Label: "Refunds", Requester: owner}); err != nil {
		t.Fatal(err)
	}
	for id, r := range map[string]drafts.Record{
		"drf_0":        {AgentID: "agt_summary", Secret: "kiff_claim_x", Label: "Day summary"},
		"drf_claimed":  {AgentID: "agt_done", Claimed: true, KeyRef: "ref"},
		"drf_mismatch": {AgentID: "agt_bad", Mismatch: true, Key: "kiff_key_y"},
		"drf_noagent":  {Secret: "kiff_claim_w", Label: "No agent"},
	} {
		if err := m.Put(ctx, "t1", id, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Put(ctx, "t2", "drf_9", drafts.Record{AgentID: "agt_other", Secret: "kiff_claim_z"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Unclaimed(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	want := []drafts.Unclaimed{{ID: "drf_0", AgentID: "agt_summary", Label: "Day summary"}, {ID: "drf_1", AgentID: "agt_refunds", Label: "Refunds", Requester: owner}}
	if len(got) != len(want) {
		t.Fatalf("listed %+v", got)
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].AgentID != want[i].AgentID || got[i].Label != want[i].Label ||
			(got[i].Requester == nil) != (want[i].Requester == nil) || got[i].Requester != nil && *got[i].Requester != *want[i].Requester {
			t.Fatalf("listed %+v, want %+v", got, want)
		}
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "kiff_claim") || strings.Contains(string(b), "kiff_key") {
		t.Fatalf("a secret or key was listed: %s", b)
	}
	// A store that can't list: ErrNoList, never an empty list taken as none.
	if _, err := (&drafts.Service{Store: getOnly{m}}).Unclaimed(ctx, "t1"); !errors.Is(err, drafts.ErrNoList) {
		t.Fatalf("a store that can't list: %v", err)
	}
}

// getOnly is a Store that can't list.
type getOnly struct{ m *memory.Drafts }

func (g getOnly) Put(c context.Context, tenant, draft string, r drafts.Record) error {
	return g.m.Put(c, tenant, draft, r)
}
func (g getOnly) Get(c context.Context, tenant, draft string) (drafts.Record, error) {
	return g.m.Get(c, tenant, draft)
}
