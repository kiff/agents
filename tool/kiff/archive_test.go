package kiff_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
