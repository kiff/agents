package kiffapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kiff/agents/drafts"
	"github.com/kiff/agents/drafts/kiffapi"
)

// KIFF's claim takes the secret as its bearer and nothing else; its 409 and
// 410 are the draft's state, not failures.
func TestClaim(t *testing.T) {
	status := http.StatusOK
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.Method != "POST" || r.URL.Path != "/v1/agent-drafts/drf_1/claim" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			w.Write([]byte(`{"agent_id":"agt_r","key_id":"k1","plaintext":"kiff_key_new","card_ids":["c1"]}`))
		} else {
			w.Write([]byte(`{"error":"the draft is being confirmed, or was already confirmed, claimed or discarded"}`))
		}
	}))
	defer srv.Close()
	c := kiffapi.New(srv.URL, nil)
	got, err := c.Claim(context.Background(), "drf_1", "kiff_claim_x")
	if err != nil || got.Key != "kiff_key_new" || got.AgentID != "agt_r" || auth != "Bearer kiff_claim_x" {
		t.Fatalf("claim: %+v %v (auth %q)", got, err, auth)
	}
	status = http.StatusConflict
	if _, err := c.Claim(context.Background(), "drf_1", "kiff_claim_x"); !errors.Is(err, drafts.ErrNotClaimable) {
		t.Fatalf("409: %v", err)
	}
	status = http.StatusGone
	if _, err := c.Claim(context.Background(), "drf_1", "kiff_claim_x"); !errors.Is(err, drafts.ErrExpired) {
		t.Fatalf("410: %v", err)
	}
}
