package main

import (
	"context"
	"errors"
	"testing"
)

// The credentials come from the secret STRIPEMCP_SECRET names, both of
// them, when it's set: a secret that can't be read or lacks either stops the
// server, and the environment's values are never used beside it.
func TestTheCredentialsComeFromTheSecretWhenItIsNamed(t *testing.T) {
	env := func(secret string) func(string) string {
		return func(k string) string {
			return map[string]string{"STRIPEMCP_SECRET": secret, "STRIPE_API_KEY": "sk_test_fromenv", "STRIPEMCP_TOKEN": "token-from-env"}[k]
		}
	}
	holding := func(raw string, err error) func(context.Context, string) (string, error) {
		return func(_ context.Context, name string) (string, error) {
			if name != "agents-eval-stripemcp" {
				t.Fatalf("read %q", name)
			}
			return raw, err
		}
	}
	key, token, err := credentials(t.Context(), env("agents-eval-stripemcp"), holding(`{"stripe_api_key":"sk_test_fromsecret","token":"token-from-secret"}`, nil))
	if err != nil || key != "sk_test_fromsecret" || token != "token-from-secret" {
		t.Fatalf("from the secret: %q %q %v", key, token, err)
	}
	for name, read := range map[string]func(context.Context, string) (string, error){
		"unreadable":   holding("", errors.New("denied")),
		"not json":     holding("sk_test_x", nil),
		"no key":       holding(`{"token":"token-from-secret"}`, nil),
		"no token":     holding(`{"stripe_api_key":"sk_test_fromsecret"}`, nil),
		"blank values": holding(`{"stripe_api_key":" ","token":" "}`, nil),
	} {
		if key, token, err := credentials(t.Context(), env("agents-eval-stripemcp"), read); err == nil || key != "" || token != "" {
			t.Errorf("%s: %q %q, no error", name, key, token)
		}
	}
	// No secret named: the environment, as when run locally; nothing read.
	key, token, err = credentials(t.Context(), env(""), func(context.Context, string) (string, error) {
		t.Fatal("read a secret no one named")
		return "", nil
	})
	if err != nil || key != "sk_test_fromenv" || token != "token-from-env" {
		t.Fatalf("from the environment: %q %q %v", key, token, err)
	}
}
