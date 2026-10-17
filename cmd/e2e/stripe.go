package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// The scenario against Stripe itself, in test mode (on demand, never
// nightly): the vendor is Stripe's MCP server, a person completes Stripe's
// sign-in, and Stripe's API says what was refunded.

// stripeMCP is Stripe's MCP server.
const stripeMCP = "https://mcp.stripe.com"

// stripe is Stripe's REST API with a test-mode key, for what the test
// sets up and checks; the taf never has it.
type stripe struct {
	key string
	hc  *http.Client
}

func newStripe(key string, hc *http.Client) (*stripe, error) {
	if !strings.HasPrefix(key, "sk_test_") && !strings.HasPrefix(key, "rk_test_") {
		return nil, errors.New("E2E_STRIPE_KEY must be a test-mode key (sk_test_ or rk_test_): this test never touches live mode")
	}
	return &stripe{key: key, hc: hc}, nil
}

func (s *stripe) do(ctx context.Context, method, path string, form url.Values) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, method, "https://api.stripe.com"+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(s.key, "")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("stripe %s %s: %d %v", method, path, resp.StatusCode, out["error"])
	}
	return out, nil
}

// payment makes a test payment of 20.00 EUR to refund, and names the
// account (the grant pins its tools to it).
func (s *stripe) payment(ctx context.Context) (account, intent string, err error) {
	a, err := s.do(ctx, http.MethodGet, "/v1/account", nil)
	if err != nil {
		return "", "", err
	}
	account, _ = a["id"].(string)
	pi, err := s.do(ctx, http.MethodPost, "/v1/payment_intents", url.Values{"amount": {"2000"}, "currency": {"eur"},
		"payment_method": {"pm_card_visa"}, "confirm": {"true"}, "automatic_payment_methods[enabled]": {"true"},
		"automatic_payment_methods[allow_redirects]": {"never"}, "description": {"kiff/agents e2e"}})
	if err != nil {
		return "", "", err
	}
	intent, _ = pi["id"].(string)
	return account, intent, nil
}

// refunds are the amounts refunded on the payment, by Stripe's account.
func (s *stripe) refunds(ctx context.Context, intent string) ([]int64, error) {
	r, err := s.do(ctx, http.MethodGet, "/v1/refunds?payment_intent="+url.QueryEscape(intent), nil)
	if err != nil {
		return nil, err
	}
	data, _ := r["data"].([]any)
	out := make([]int64, 0, len(data))
	for _, d := range data {
		if m, ok := d.(map[string]any); ok {
			if a, ok := m["amount"].(float64); ok {
				out = append(out, int64(a))
			}
		}
	}
	return out, nil
}

// stripeAsk is the owner's first message in the setup chat, against
// Stripe: the outcome, in their words, as against the fake vendor.
func stripeAsk() string {
	return "I want our customers to be able to get their refunds. We take payments with Stripe, in test mode for now; " +
		"its MCP server is " + stripeMCP + ". Call the taf Refunds."
}

// checkStripeGrant is what Setup chose from Stripe's generic tools: the
// write tool narrowed to refunds, pinned to this account and to test mode.
func checkStripeGrant(g grant, account string) error {
	for _, t := range g.Request.Tools {
		f := t.Fixed
		if t.Tool == "stripe_api_write" && f["stripe_api_operation_id"] == "PostRefunds" && f["stripe_context"] == account && f["livemode"] == false {
			return nil
		}
	}
	b, _ := json.Marshal(g.Request.Tools)
	return fmt.Errorf("Setup's grant isn't stripe_api_write pinned to PostRefunds, this account and test mode: %s", b)
}

// waitForSignIn is a person signing in at the vendor: the link is printed,
// and the app's link the sign-in ended at (tafs://planque/signed-in?…) is
// read from file once they write it there.
func waitForSignIn(file string, say func(string, ...any)) func(context.Context, string) (string, error) {
	return func(ctx context.Context, link string) (string, error) {
		if link == "" {
			return "", errors.New("approve gave no sign-in link")
		}
		_ = os.Remove(file)
		say("sign in at the vendor within 10 minutes, then write the tafs:// link it ends at to %s:\n    %s", file, link)
		for deadline := time.Now().Add(10 * time.Minute); time.Now().Before(deadline); {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(2 * time.Second):
			}
			b, err := os.ReadFile(file)
			if err != nil {
				continue
			}
			loc, err := url.Parse(strings.TrimSpace(string(b)))
			if err != nil || loc.Scheme != "tafs" {
				return "", fmt.Errorf("%s holds no tafs:// link", file)
			}
			if code := loc.Query().Get("confirm"); code != "" {
				return code, nil
			}
			return "", errors.New("the app's link carries no confirmation code")
		}
		return "", errors.New("nobody signed in at the vendor in time")
	}
}
