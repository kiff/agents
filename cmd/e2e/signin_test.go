package main

import (
	"github.com/kiff/agents/cmd/internal/testaccount"

	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeSignInKIFF is KIFF's device sign-in, approved at once on account, the
// setup agent's draft confirmed on submit, and its key claimed with no roles.
func fakeSignInKIFF(t *testing.T, account string) (*httptest.Server, *int, *[]string) {
	t.Helper()
	drafts := 0
	var signedOut []string
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
		case "/v1/auth/device/logout":
			signedOut = append(signedOut, r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusNoContent)
		case "/v1/agent-drafts/drf_1/claim":
			_ = json.NewEncoder(w).Encode(map[string]any{"plaintext": "setup-key", "roles": []any{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &drafts, &signedOut
}

// The test signs in on its own account with a new phone key: the file gets
// the session, the setup agent's key and the phone key, the screen none.
func TestTheTestSignsInOnItsOwnAccount(t *testing.T) {
	devicePoll = time.Millisecond
	srv, _, signedOut := fakeSignInKIFF(t, "t-test")
	var out, say bytes.Buffer
	if err := kiffSignIn(context.Background(), srv.URL, "t-test", "e2e-setup", nil, srv.Client(), &out, &say); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 3 || lines[0] != "sess-e2e" || lines[1] != "setup-key" {
		t.Fatalf("wrote %q", out.String())
	}
	if _, err := testaccount.ParsePhone(lines[2]); err != nil {
		t.Fatalf("the phone key doesn't parse: %v", err)
	}
	if len(*signedOut) != 0 {
		t.Fatalf("signed out the session it keeps: %v", *signedOut)
	}
	for _, secret := range lines {
		if strings.Contains(say.String(), secret) {
			t.Fatalf("printed a secret: %q", say.String())
		}
	}
}

// Approved on an account people use, or with no account named, nothing is
// written, no setup agent is made there, and the session KIFF gave is signed
// out, so it isn't left live on that account.
func TestTheTestsSignInOnAnotherAccountWritesNothing(t *testing.T) {
	devicePoll = time.Millisecond
	srv, drafts, signedOut := fakeSignInKIFF(t, "t-people")
	var out, say bytes.Buffer
	err := kiffSignIn(context.Background(), srv.URL, "t-test", "e2e-setup", nil, srv.Client(), &out, &say)
	if err == nil || !strings.Contains(err.Error(), "another account") || out.Len() != 0 || *drafts != 0 {
		t.Fatalf("err %v, wrote %q, drafts %d", err, out.String(), *drafts)
	}
	if want := []string{"Bearer sess-e2e"}; !slices.Equal(*signedOut, want) {
		t.Fatalf("signed out %v, want %v", *signedOut, want)
	}
	if err := kiffSignIn(context.Background(), srv.URL, "", "e2e-setup", nil, srv.Client(), &out, &say); err == nil || out.Len() != 0 {
		t.Fatalf("no account: err %v, wrote %q", err, out.String())
	}
}

// A robot test user's sign-in: the key's fingerprints are printed first (for
// KIFF_PROOF_TEST_KEYS), the file gets only the session and the key, no
// agent is drafted, and nothing secret reaches the screen.
func TestThePilotSignsInWithAKeyItPrintsAndMakesNoAgent(t *testing.T) {
	devicePoll = time.Millisecond
	srv, drafts, signedOut := fakeSignInKIFF(t, "t-pilot")
	var out, say bytes.Buffer
	if err := pilotSignIn(context.Background(), srv.URL, "t-pilot", nil, srv.Client(), &out, &say); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 || lines[0] != "sess-e2e" {
		t.Fatalf("wrote %q", out.String())
	}
	key, err := testaccount.ParsePhone(lines[1])
	if err != nil {
		t.Fatalf("the phone key doesn't parse: %v", err)
	}
	short, full, err := keyFingerprints(key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(say.String(), short) || !strings.Contains(say.String(), full) {
		t.Fatalf("the fingerprints of the key it wrote aren't printed: %q", say.String())
	}
	if *drafts != 0 || len(*signedOut) != 0 {
		t.Fatalf("drafted %d agents, signed out %v", *drafts, *signedOut)
	}
	for _, secret := range lines {
		if strings.Contains(say.String(), secret) {
			t.Fatalf("printed a secret: %q", say.String())
		}
	}
}

// Approved on another account, or with none named, the pilot's sign-in
// writes nothing and signs out the session KIFF gave.
func TestThePilotsSignInOnAnotherAccountWritesNothing(t *testing.T) {
	devicePoll = time.Millisecond
	srv, drafts, signedOut := fakeSignInKIFF(t, "t-people")
	var out, say bytes.Buffer
	err := pilotSignIn(context.Background(), srv.URL, "t-pilot", nil, srv.Client(), &out, &say)
	if err == nil || !strings.Contains(err.Error(), "another account") || out.Len() != 0 || *drafts != 0 {
		t.Fatalf("err %v, wrote %q, drafts %d", err, out.String(), *drafts)
	}
	if want := []string{"Bearer sess-e2e"}; !slices.Equal(*signedOut, want) {
		t.Fatalf("signed out %v, want %v", *signedOut, want)
	}
	if err := pilotSignIn(context.Background(), srv.URL, "", nil, srv.Client(), &out, &say); err == nil || out.Len() != 0 {
		t.Fatalf("no account: err %v, wrote %q", err, out.String())
	}
}

// On an account that's already a test account, the key comes first: -phone-key
// writes a key and prints only its fingerprints, and -signin then signs in
// with that same key, writing it back as the phone key.
func TestAKeyMadeFirstIsTheOneSignedInWith(t *testing.T) {
	devicePoll = time.Millisecond
	var keyOut, say bytes.Buffer
	if err := phoneKey(&keyOut, &say); err != nil {
		t.Fatal(err)
	}
	key, err := testaccount.ParsePhone(strings.TrimSpace(keyOut.String()))
	if err != nil {
		t.Fatalf("the key doesn't parse: %v", err)
	}
	short, full, _ := keyFingerprints(key)
	if !strings.Contains(say.String(), short) || !strings.Contains(say.String(), full) || strings.Contains(say.String(), strings.TrimSpace(keyOut.String())) {
		t.Fatalf("printed %q", say.String())
	}
	srv, _, _ := fakeSignInKIFF(t, "t-test")
	var out bytes.Buffer
	if err := kiffSignIn(context.Background(), srv.URL, "t-test", "e2e-setup", key, srv.Client(), &out, &say); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 3 || lines[2] != strings.TrimSpace(keyOut.String()) {
		t.Fatalf("the sign-in didn't keep the key made first: %q", out.String())
	}
	var pilotOut bytes.Buffer
	srv2, _, _ := fakeSignInKIFF(t, "t-pilot")
	if err := pilotSignIn(context.Background(), srv2.URL, "t-pilot", key, srv2.Client(), &pilotOut, &say); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSuffix(pilotOut.String(), "\n"), "\n"); len(lines) != 2 || lines[1] != strings.TrimSpace(keyOut.String()) {
		t.Fatalf("the pilot's sign-in didn't keep the key made first: %q", pilotOut.String())
	}
}
