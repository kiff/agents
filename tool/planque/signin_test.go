package planque_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/tool/planque"
)

type signInAgent struct{ registry.Agents }

func (signInAgent) Get(_ context.Context, tenant, id string) (registry.Definition, error) {
	if tenant != "t1" || id != "files" {
		return registry.Definition{}, registry.ErrNotFound
	}
	return registry.Definition{Tenant: tenant, ID: id, KeyRef: "files-key"}, nil
}

type signInKeys struct{}

func (signInKeys) Key(_ context.Context, tenant, ref string) (string, error) {
	if tenant != "t1" || ref != "files-key" {
		return "", errors.New("no such key")
	}
	return "kiff_k_files", nil
}

// The check asks planque with the taf's own key, sends nothing but the grant,
// and reads only the verdict: signed_out holds, anything else runs.
func TestASignInCheckAsksPlanqueWithTheTafsOwnKey(t *testing.T) {
	answer := `{"grant":"grt_b1","sites":[{"site":"books.example","verdict":"signed_in"},{"site":"supplier-portal.example","verdict":"signed_out","checked_at":"2026-11-28T06:55:00Z"}]}`
	status := http.StatusOK
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" body="+string(body))
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	defer srv.Close()
	check := planque.SignInChecker(signInAgent{}, signInKeys{}, srv.Client(), srv.URL)
	stopped, err := check(context.Background(), "t1", "files", "grt_b1")
	if err != nil || stopped == nil || stopped.Site != "supplier-portal.example" {
		t.Fatalf("stopped %+v, %v", stopped, err)
	}
	if len(got) != 1 || got[0] != "POST /v1/accounts/t1/grants/grt_b1/health Bearer kiff_k_files body=" {
		t.Fatalf("asked %v", got)
	}
	// Working or unclear: nothing holds.
	answer = `{"sites":[{"site":"a.example","verdict":"signed_in"},{"site":"b.example","verdict":"unknown"}]}`
	if stopped, err := check(context.Background(), "t1", "files", "grt_b1"); err != nil || stopped != nil {
		t.Fatalf("working: %+v %v", stopped, err)
	}
	// planque refusing or down is an error, which the schedule runs past.
	status = http.StatusServiceUnavailable
	if _, err := check(context.Background(), "t1", "files", "grt_b1"); err == nil || strings.Contains(err.Error(), "kiff_k_files") {
		t.Fatalf("a refusal: %v", err)
	}
	// Another agent's key is never used.
	if _, err := check(context.Background(), "t1", "other", "grt_b1"); err == nil {
		t.Fatal("an agent with no definition was checked")
	}
}
