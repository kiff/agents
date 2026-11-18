package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kiff/agents/server"
)

// keyWrites records what reaches the keys store.
type keyWrites struct {
	mu    sync.Mutex
	calls []string
}

func (k *keyWrites) Put(_ context.Context, tenant, ref, key string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, "put "+tenant+" "+ref+" "+key)
	return nil
}

func (k *keyWrites) Remove(_ context.Context, tenant, ref string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, "remove "+tenant+" "+ref)
	return nil
}

const tafsCloud = "arn:aws:sts::000000000000:assumed-role/tafs-cloud/"

func keysServer(t *testing.T, k *keyWrites) *httptest.Server {
	t.Helper()
	s := &server.Server{Keys: k, KeyWriters: []string{tafsCloud}, Auth: func(r *http.Request) (server.Caller, error) {
		return server.Caller{Tenant: r.Header.Get("Test-Tenant"), Role: r.Header.Get("Test-Role")}, nil
	}}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func keyCall(t *testing.T, srv *httptest.Server, method, path, role, body string) int {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Test-Tenant", "t1")
	req.Header.Set("Test-Role", role)
	// A path with "." or ".." segments, or "//", is answered with a redirect
	// to its clean form (http.ServeMux), never served: it isn't followed here.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

// Only tafs cloud's role writes a key: another caller of the API (an agent
// run never holds one; agents' own role or a console's would) is refused,
// and nothing reaches the store.
func TestOnlyTheKeyWritersRoleWritesKeys(t *testing.T) {
	k := &keyWrites{}
	srv := keysServer(t, k)
	for _, role := range []string{"", "arn:aws:sts::000000000000:assumed-role/agents-lambda/", "arn:aws:sts::000000000000:assumed-role/console/"} {
		if got := keyCall(t, srv, "PUT", "/tenant/keys/kiff/taf_1", role, `{"key":"kiff_key_x"}`); got != http.StatusUnauthorized {
			t.Errorf("PUT as %q: %d", role, got)
		}
		if got := keyCall(t, srv, "DELETE", "/tenant/keys/kiff/taf_1", role, ""); got != http.StatusUnauthorized {
			t.Errorf("DELETE as %q: %d", role, got)
		}
	}
	if len(k.calls) != 0 {
		t.Fatalf("reached the store: %v", k.calls)
	}
	if got := keyCall(t, srv, "PUT", "/tenant/keys/kiff/taf_1", tafsCloud, `{"key":"kiff_key_x"}`); got != http.StatusNoContent {
		t.Fatalf("PUT as tafs cloud: %d", got)
	}
	if got := keyCall(t, srv, "DELETE", "/tenant/keys/kiff/taf_1", tafsCloud, ""); got != http.StatusNoContent {
		t.Fatalf("DELETE as tafs cloud: %d", got)
	}
	if want := []string{"put t1 kiff/taf_1 kiff_key_x", "remove t1 kiff/taf_1"}; strings.Join(k.calls, ";") != strings.Join(want, ";") {
		t.Fatalf("store calls %v, want %v", k.calls, want)
	}
}

// The tenant is the signed one: a body naming another is refused, and a
// reference can't climb out of the tenant's keys (a path that tries is only
// redirected to its clean form, which is the same tenant's).
func TestAKeysTenantIsTheSignedOne(t *testing.T) {
	k := &keyWrites{}
	srv := keysServer(t, k)
	for _, c := range []struct{ method, path, body string }{
		{"PUT", "/tenant/keys/kiff/taf_1", `{"key":"kiff_key_x","tenant":"t2"}`},
		{"PUT", "/tenant/keys/kiff/taf_1", `{"key":"  "}`},
		{"PUT", "/tenant/keys/kiff/taf_1", `not json`},
		{"PUT", "/tenant/keys/kiff/../t2/kiff/taf_1", `{"key":"kiff_key_x"}`},
		{"PUT", "/tenant/keys/kiff//taf_1", `{"key":"kiff_key_x"}`},
		{"DELETE", "/tenant/keys/kiff/%2e%2e", ""},
	} {
		if got := keyCall(t, srv, c.method, c.path, tafsCloud, c.body); got < 300 || got >= 500 {
			t.Errorf("%s %s %s: %d", c.method, c.path, c.body, got)
		}
	}
	if len(k.calls) != 0 {
		t.Fatalf("reached the store: %v", k.calls)
	}
}

// Without a keys store there are no key routes.
func TestNoKeyRoutesWithoutAStore(t *testing.T) {
	s := &server.Server{Auth: func(*http.Request) (server.Caller, error) { return server.Caller{Tenant: "t1", Role: tafsCloud}, nil }}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	if got := keyCall(t, srv, "PUT", "/tenant/keys/kiff/taf_1", tafsCloud, `{"key":"k"}`); got != http.StatusNotFound {
		t.Fatalf("%d", got)
	}
}
