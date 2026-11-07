package kiff_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/registry/secretsmanager"
	"github.com/kiff/agents/tool/kiff"
)

// ArchiveSelf posts to KIFF's self archive with the agent's own key. An
// archived agent, a key KIFF already revoked (401, 403) and a KIFF without
// the route (404) are done; anything else is an error without the key in it.
func TestArchiveSelf(t *testing.T) {
	var status int
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"` + testKey + `"}`))
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()
	for _, s := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		status = s
		if err := kiff.ArchiveSelf(ctx, srv.Client(), srv.URL+"/", testKey); err != nil {
			t.Errorf("KIFF answered %d: %v, want done", s, err)
		}
	}
	if seen[0] != "POST /v1/agents/self/archive Bearer "+testKey {
		t.Fatalf("asked %q", seen[0])
	}
	for _, s := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusConflict} {
		status = s
		err := kiff.ArchiveSelf(ctx, srv.Client(), srv.URL, testKey)
		if err == nil || strings.Contains(err.Error(), testKey) {
			t.Errorf("KIFF answered %d: %v, want an error without the key", s, err)
		}
	}
}

// goneKeys has lost every key, as when tafs cloud deleted the taf's secret.
type goneKeys struct{}

func (goneKeys) Key(context.Context, string, string) (string, error) {
	return "", secretsmanager.ErrNotFound
}

// Archiver archives with the agent's own key; an agent already gone, or
// whose key secret is gone, is done without asking KIFF, so a removal
// asked again still finishes. Any other failure to read the key is an
// error.
func TestArchiverUsesTheAgentsKey(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path+" "+r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()
	if err := kiff.Archiver(oneAgent{}, keys{}, srv.Client(), srv.URL)(ctx, "t1", "rae"); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != "/v1/agents/self/archive Bearer "+testKey {
		t.Fatalf("asked %v", asked)
	}
	asked = nil
	if err := kiff.Archiver(oneAgent{}, goneKeys{}, srv.Client(), srv.URL)(ctx, "t1", "rae"); err != nil {
		t.Fatalf("the key secret gone: %v, want done", err)
	}
	if err := kiff.Archiver(oneAgent{}, keys{}, srv.Client(), srv.URL)(ctx, "t2", "rae"); err != nil {
		t.Fatalf("the agent gone: %v, want done", err)
	}
	if len(asked) != 0 {
		t.Fatalf("asked KIFF with no key: %v", asked)
	}
	// An agent with no key named: nothing to archive with, KIFF unasked.
	if err := kiff.Archiver(noKeyAgent{}, keys{}, srv.Client(), srv.URL)(ctx, "t1", "rae"); err != nil || len(asked) != 0 {
		t.Fatalf("no key named: %v, asked %v", err, asked)
	}
	// A key that can't be read for another reason is an error.
	if err := kiff.Archiver(oneAgent{}, failingKeys{}, srv.Client(), srv.URL)(ctx, "t1", "rae"); err == nil {
		t.Fatal("an unreadable key: no error")
	}
}

type failingKeys struct{}

func (failingKeys) Key(context.Context, string, string) (string, error) {
	return "", errors.New("secrets manager down")
}

// noKeyAgent is t1's "rae", with no KIFF key named.
type noKeyAgent struct{ registry.Agents }

func (noKeyAgent) Get(_ context.Context, tenant, id string) (registry.Definition, error) {
	return registry.Definition{Tenant: tenant, ID: id}, nil
}
