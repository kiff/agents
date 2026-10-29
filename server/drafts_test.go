package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kiff/agents/drafts"
	"github.com/kiff/agents/server"
)

// claims answers a claim by draft id, and records who asked.
type claims struct{ asked []string }

func (c *claims) Claim(_ context.Context, tenant, draft, ref string) (string, error) {
	c.asked = append(c.asked, tenant+" "+draft+" "+ref)
	switch draft {
	case "drf_ok":
		return "agt_refunds", nil
	case "drf_wait":
		return "", drafts.ErrNotClaimable
	case "drf_other":
		return "", drafts.ErrOtherRef
	case "drf_taken":
		return "", drafts.ErrTaken
	case "drf_old":
		return "", drafts.ErrExpired
	}
	return "", drafts.ErrNotFound
}

// The claim route takes the tenant from the verified caller, never the
// body, and answers each state with what to do.
func TestClaimRoute(t *testing.T) {
	c := &claims{}
	s := &server.Server{Drafts: c, Auth: func(r *http.Request) (server.Caller, error) {
		if t := r.Header.Get("X-Test-Tenant"); t != "" {
			return server.Caller{Tenant: t}, nil
		}
		return server.Caller{}, errors.New("unverified")
	}}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	post := func(tenant, draft, body string) (int, string) {
		req, _ := http.NewRequest("POST", srv.URL+"/drafts/"+draft+"/claim", strings.NewReader(body))
		if tenant != "" {
			req.Header.Set("X-Test-Tenant", tenant)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if st, body := post("t1", "drf_ok", `{"key_ref":"kiff/taf-1"}`); st != 200 || !strings.Contains(body, `"agent_id":"agt_refunds"`) {
		t.Fatalf("ok: %d %s", st, body)
	}
	if c.asked[0] != "t1 drf_ok kiff/taf-1" {
		t.Fatalf("asked %v", c.asked)
	}
	// Each refusal has its own code, which the caller switches on, and words.
	for draft, want := range map[string]struct {
		status int
		code   string
	}{"drf_wait": {409, "draft_not_ready"}, "drf_other": {409, "draft_claimed"}, "drf_taken": {409, "key_ref_taken"}, "drf_old": {409, "draft_expired"}, "drf_none": {404, "not_found"}} {
		st, body := post("t1", draft, `{"key_ref":"kiff/taf-1"}`)
		var e map[string]string
		_ = json.Unmarshal([]byte(body), &e)
		if st != want.status || e["error"] != want.code || e["message"] == "" {
			t.Errorf("%s: %d %s, want %d with code %s and words", draft, st, body, want.status, want.code)
		}
	}
	n := len(c.asked)
	for _, tc := range []struct{ tenant, body string }{{"", `{"key_ref":"kiff/taf-1"}`}, {"t1", `{"key_ref":""}`}, {"t1", `{"key_ref":"x","tenant":"t2"}`}} {
		if st, _ := post(tc.tenant, "drf_ok", tc.body); st != 401 && st != 400 {
			t.Errorf("%q %s: %d", tc.tenant, tc.body, st)
		}
	}
	if len(c.asked) != n {
		t.Fatalf("a refused request reached the claim: %v", c.asked[n:])
	}
}

// Without drafts configured, the route isn't served.
func TestNoClaimRouteWithoutDrafts(t *testing.T) {
	s := &server.Server{Auth: func(*http.Request) (server.Caller, error) { return server.Caller{Tenant: "t1"}, nil }}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/drafts/drf_ok/claim", "application/json", strings.NewReader(`{"key_ref":"k"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 404 && resp.StatusCode != 405 {
		t.Fatalf("served without drafts: %d", resp.StatusCode)
	}
}
