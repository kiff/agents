package kiff_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/tool/kiff"
)

// api is a fake KIFF API with the withdraw route, answering as kiff-cloud's
// withdrawHold does: exc_held is withdrawn (200, and 200 again), exc_sent was
// already sent (409, status approved), anything else is 404.
func api(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			http.Error(w, `{"error":"missing Authorization header"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/me/holds/exc_held/withdraw":
			_, _ = w.Write([]byte(`{"exception_id":"exc_held","action":"stripe.refund","status":"invalidated","withdrawn":true}`))
		case "POST /v1/me/holds/exc_sent/withdraw":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"already_answered","detail":"this call was already sent, refused or re-decided, or it expired; nothing was changed",` +
				`"hold":{"exception_id":"exc_sent","action":"stripe.refund","status":"approved"}}`))
		case "POST /v1/me/holds/exc_busy/withdraw":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"in_flight","hold":{"exception_id":"exc_busy","status":"approved"}}`))
		case "POST /v1/me/holds/exc_bare/withdraw":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"already_answered"}`))
		case "POST /v1/me/holds/exc_down/withdraw":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"withdraw failed"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no such held call"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A held call is withdrawn, and asking again answers the same.
func TestWithdrawAHeldCall(t *testing.T) {
	srv := api(t)
	for range 2 {
		got, err := kiff.Withdraw(context.Background(), srv.Client(), srv.URL, testKey, "exc_held")
		if err != nil || !got.Withdrawn || got.Status != "invalidated" {
			t.Fatalf("got %+v, %v", got, err)
		}
	}
}

// A call already sent, refused or expired isn't withdrawn: Withdraw says
// where it stands, so the run can record what KIFF did.
func TestACallAlreadyAnsweredIsNotWithdrawn(t *testing.T) {
	srv := api(t)
	got, err := kiff.Withdraw(context.Background(), srv.Client(), srv.URL, testKey, "exc_sent")
	if err != nil || got.Withdrawn || got.Status != "approved" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// A hold KIFF doesn't know for this agent is ErrNoHold; any other failure is
// an error, and no error carries the key.
func TestWithdrawFailures(t *testing.T) {
	srv := api(t)
	if _, err := kiff.Withdraw(context.Background(), srv.Client(), srv.URL, testKey, "exc_other"); !errors.Is(err, kiff.ErrNoHold) {
		t.Fatalf("an unknown hold: err = %v, want ErrNoHold", err)
	}
	for _, ex := range []string{"exc_busy", "exc_bare"} { // a 409 that isn't already_answered, or names no status
		if w, err := kiff.Withdraw(context.Background(), srv.Client(), srv.URL, testKey, ex); err == nil {
			t.Fatalf("%s: %+v, want an error: KIFF didn't say what became of the call", ex, w)
		}
	}
	if _, err := kiff.Withdraw(context.Background(), srv.Client(), srv.URL, testKey, "exc_down"); err == nil || errors.Is(err, kiff.ErrNoHold) {
		t.Fatalf("a server error: err = %v", err)
	}
	if _, err := kiff.Withdraw(context.Background(), srv.Client(), srv.URL, "wrong-"+testKey, "exc_held"); err == nil || strings.Contains(err.Error(), testKey) {
		t.Fatalf("a refused key: err = %v", err)
	}
	if _, err := kiff.Withdraw(context.Background(), srv.Client(), "http://127.0.0.1:1/"+testKey, testKey, "exc_held"); err == nil || strings.Contains(err.Error(), testKey) {
		t.Fatalf("an unreachable API: err = %v, want an error without the key", err)
	}
	if _, err := kiff.Withdraw(context.Background(), srv.Client(), srv.URL, testKey, ""); err == nil {
		t.Fatal("no exception: no error")
	}
}

// oneAgent is t1's agent "rae", whose KIFF key is under "rae-key".
type oneAgent struct{ registry.Agents }

func (oneAgent) Get(_ context.Context, tenant, id string) (registry.Definition, error) {
	if tenant != "t1" || id != "rae" {
		return registry.Definition{}, registry.ErrNotFound
	}
	return registry.Definition{Tenant: tenant, ID: id, KeyRef: "rae-key"}, nil
}

type keys struct{}

func (keys) Key(_ context.Context, tenant, ref string) (string, error) {
	if tenant != "t1" || ref != "rae-key" {
		return "", errors.New("no such key")
	}
	return testKey, nil
}

// Withdrawer withdraws with the run's own agent's key, and says what became
// of the call.
func TestWithdrawerUsesTheRunsAgentsKey(t *testing.T) {
	srv := api(t)
	withdraw := kiff.Withdrawer(oneAgent{}, keys{}, srv.Client(), srv.URL)
	r := run.Run{Tenant: "t1", Agent: "rae", ID: "r1"}
	for ex, want := range map[string]string{"exc_held": "invalidated", "exc_sent": "approved"} {
		if got, err := withdraw(context.Background(), r, ex); err != nil || got != want {
			t.Fatalf("%s: %q, %v; want %q", ex, got, err, want)
		}
	}
	if _, err := withdraw(context.Background(), r, "exc_other"); !errors.Is(err, kiff.ErrNoHold) {
		t.Fatalf("an unknown hold: %v", err)
	}
	if _, err := withdraw(context.Background(), run.Run{Tenant: "t2", Agent: "rae", ID: "r1"}, "exc_held"); err == nil ||
		errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("another tenant's agent: %v, want an error that isn't the API's 404", err)
	}
}
