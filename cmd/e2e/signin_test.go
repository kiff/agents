package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeSignInKIFF is KIFF's device sign-in, approved at once on account, the
// setup agent's draft confirmed on submit, and its key claimed with no roles.
func fakeSignInKIFF(t *testing.T, account string) (*httptest.Server, *int) {
	t.Helper()
	drafts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/device/start":
			_ = json.NewEncoder(w).Encode(map[string]string{"device_code": "dc", "verification_uri_complete": "https://kiff.test/device?c=1"})
		case "/v1/auth/device/token":
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "sess-e2e", "tenant": account})
		case "/v1/me/agent-drafts":
			drafts++
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "drf_1", "claim_secret": "cs", "status": "confirmed"})
		case "/v1/agent-drafts/drf_1/claim":
			_ = json.NewEncoder(w).Encode(map[string]any{"plaintext": "setup-key", "roles": []any{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &drafts
}

// The test signs in on its own account with a new phone key: the file gets
// the session, the setup agent's key and the phone key, the screen none.
func TestTheTestSignsInOnItsOwnAccount(t *testing.T) {
	devicePoll = time.Millisecond
	srv, _ := fakeSignInKIFF(t, "t-test")
	var out, say bytes.Buffer
	if err := kiffSignIn(context.Background(), srv.URL, "t-test", "e2e-setup", srv.Client(), &out, &say); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 3 || lines[0] != "sess-e2e" || lines[1] != "setup-key" {
		t.Fatalf("wrote %q", out.String())
	}
	if _, err := parsePhone(lines[2]); err != nil {
		t.Fatalf("the phone key doesn't parse: %v", err)
	}
	for _, secret := range lines {
		if strings.Contains(say.String(), secret) {
			t.Fatalf("printed a secret: %q", say.String())
		}
	}
}

// Approved on an account people use, or with no account named, nothing is
// written and no setup agent is made there.
func TestTheTestsSignInOnAnotherAccountWritesNothing(t *testing.T) {
	devicePoll = time.Millisecond
	srv, drafts := fakeSignInKIFF(t, "t-people")
	var out, say bytes.Buffer
	err := kiffSignIn(context.Background(), srv.URL, "t-test", "e2e-setup", srv.Client(), &out, &say)
	if err == nil || !strings.Contains(err.Error(), "another account") || out.Len() != 0 || *drafts != 0 {
		t.Fatalf("err %v, wrote %q, drafts %d", err, out.String(), *drafts)
	}
	if err := kiffSignIn(context.Background(), srv.URL, "", "e2e-setup", srv.Client(), &out, &say); err == nil || out.Len() != 0 {
		t.Fatalf("no account: err %v, wrote %q", err, out.String())
	}
}
