package anthropic_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/model/anthropic"
)

// The backup takes a new run only for the credit refusal, matched on its
// status, type and words, or for the API unavailable once its retries are
// spent: never for a request it would refuse too, a refused key, or a rate
// limit (qa-kiff's list).
func TestOnlyTheCreditRefusalAndAnOutageFallBack(t *testing.T) {
	credit := `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."}}`
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"the credit refusal", 400, credit, true},
		{"another bad request", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"messages: roles must alternate"}}`, false},
		{"a context too long", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 1200000 tokens > 1000000 maximum"}}`, false},
		{"credit words with another status", 403, `{"type":"error","error":{"type":"permission_error","message":"Your credit balance is too low"}}`, false},
		{"credit words with another type", 400, `{"type":"error","error":{"type":"billing_error","message":"Your credit balance is too low"}}`, false},
		{"a refused key", 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, false},
		{"a rate limit", 429, `{"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your per-minute rate limit"}}`, false},
		{"overloaded", 529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, true},
		{"an internal error", 500, `{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`, true},
		{"unavailable", 503, `{"type":"error","error":{"type":"api_error","message":"Service unavailable"}}`, true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		}))
		m := anthropic.New(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
		_, err := m.Turn(context.Background(), model.Request{Model: "claude-sonnet-5-5", MaxTokens: 64,
			Messages: []model.Message{{Role: model.User, Blocks: []model.Block{{Kind: model.Text, Text: "Hi."}}}}}, nil)
		srv.Close()
		if err == nil {
			t.Fatalf("%s: no error", c.name)
		}
		if got := anthropic.FallsBack(err); got != c.want {
			t.Errorf("%s: FallsBack = %v, want %v (%v)", c.name, got, c.want, err)
		}
		// Only the credit refusal is ours to say so about, its words kept for the logs' alarm.
		credit := c.name == "the credit refusal"
		if errors.Is(err, model.ErrProviderBalance) != credit || credit && !strings.Contains(err.Error(), "credit balance") {
			t.Errorf("%s: ErrProviderBalance = %v, want %v (%v)", c.name, errors.Is(err, model.ErrProviderBalance), credit, err)
		}
	}
	if anthropic.FallsBack(context.Canceled) || anthropic.FallsBack(errors.New("dial tcp: connection refused")) {
		t.Error("an error that isn't the API's fell back")
	}
}
