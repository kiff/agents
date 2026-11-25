package main

import (
	"github.com/kiff/agents/cmd/internal/testaccount"

	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// signedOut is each session fakeDeviceKIFF was asked to sign out.
var signedOut []string

// fakeDeviceKIFF is KIFF's device sign-in, approved on the second ask on
// account, and /v1/me answering role there for the session it gave.
func fakeDeviceKIFF(t *testing.T, account, role string) *httptest.Server {
	t.Helper()
	asks := 0
	signedOut = nil
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/device/start":
			var b struct {
				PublicKey  string `json:"public_key"`
				DeviceName string `json:"device_name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b.PublicKey == "" || !strings.Contains(b.DeviceName, "luis") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"device_code": "dc", "verification_uri_complete": "https://kiff.test/device?c=1"})
		case "/v1/auth/device/token":
			if asks++; asks < 2 {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "sess-luis", "tenant": account})
		case "/v1/auth/device/logout":
			signedOut = append(signedOut, r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusNoContent)
		case "/v1/me":
			if r.Header.Get("Authorization") != "Bearer sess-luis" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"tenant_id": account, "tenant_display_name": "KIFF Eval", "member_role": role})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A person signs in on a new phone key and makes no agent: the file gets
// the key and the session, the screen neither (step 9).
func TestAPersonSignsInOnANewPhoneKey(t *testing.T) {
	devicePoll = time.Millisecond
	srv := fakeDeviceKIFF(t, "t-eval", "viewer")
	var out, say bytes.Buffer
	if err := peopleSignIn(context.Background(), srv.URL, "t-eval", "luis", madeKey(t), srv.Client(), &out, &say); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 || lines[1] != "sess-luis" {
		t.Fatalf("wrote %q", out.String())
	}
	if _, err := testaccount.ParsePhone(lines[0]); err != nil {
		t.Fatalf("the phone key doesn't parse: %v", err)
	}
	if len(signedOut) != 0 {
		t.Fatalf("signed out the session it keeps: %v", signedOut)
	}
	if s := say.String(); strings.Contains(s, "sess-luis") || strings.Contains(s, lines[0]) || !strings.Contains(s, `"KIFF Eval" as viewer`) {
		t.Fatalf("printed %q", s)
	}
}

// Approved on another account, or with another role, nothing is written.
func TestAPersonOnTheWrongAccountWritesNothing(t *testing.T) {
	devicePoll = time.Millisecond
	var out, say bytes.Buffer
	srv := fakeDeviceKIFF(t, "t-eval", "admin")
	err := peopleSignIn(context.Background(), srv.URL, "t-eval", "luis", madeKey(t), srv.Client(), &out, &say)
	if err == nil || !strings.Contains(err.Error(), "not as viewer") || out.Len() != 0 {
		t.Fatalf("another role: err %v, wrote %q", err, out.String())
	}
	if len(signedOut) != 1 {
		t.Fatalf("another role: signed out %v", signedOut)
	}
	// The right role, on the person's own team's account.
	srv = fakeDeviceKIFF(t, "t-own", "viewer")
	err = peopleSignIn(context.Background(), srv.URL, "t-eval", "luis", madeKey(t), srv.Client(), &out, &say)
	if err == nil || !strings.Contains(err.Error(), "not to the shared account") || out.Len() != 0 {
		t.Fatalf("another account: err %v, wrote %q", err, out.String())
	}
	if len(signedOut) != 1 {
		t.Fatalf("another account: signed out %v", signedOut)
	}
	if err := peopleSignIn(context.Background(), srv.URL, "t-eval", "ana", madeKey(t), srv.Client(), &out, &say); err == nil || out.Len() != 0 {
		t.Fatalf("an unknown person: err %v, wrote %q", err, out.String())
	}
	if err := peopleSignIn(context.Background(), srv.URL, "", "luis", madeKey(t), srv.Client(), &out, &say); err == nil || out.Len() != 0 {
		t.Fatalf("no account: err %v, wrote %q", err, out.String())
	}
}

// madeKey is a phone key as -people-key makes it, read back as -people-signin does.
func madeKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	var out, say bytes.Buffer
	if err := peopleKey("luis", &out, &say); err != nil {
		t.Fatal(err)
	}
	k, err := testaccount.ParsePhone(out.String())
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// The key comes first, and alone: -people-key writes it and prints only its
// fingerprints, short and whole, for KIFF's test keys; -people-signin signs in
// with that key, never a new one, and with none it signs in with nothing.
func TestThePhoneKeyIsMadeBeforeTheSignInAndSignedInWith(t *testing.T) {
	var out, say bytes.Buffer
	if err := peopleKey("maria", &out, &say); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSuffix(out.String(), "\n")
	key, err := testaccount.ParsePhone(line)
	if err != nil || strings.Contains(line, "\n") {
		t.Fatalf("the key file: %q %v", out.String(), err)
	}
	short, full, _ := keyFingerprints(key)
	if s := say.String(); strings.Contains(s, line) || !strings.Contains(s, short) || !strings.Contains(s, full) || len(full) != 64 || len(short) != 19 {
		t.Fatalf("printed %q (short %q, full %q)", s, short, full)
	}
	if err := peopleKey("ana", &out, &say); err == nil {
		t.Fatal("an unknown person")
	}

	devicePoll = time.Millisecond
	srv := fakeDeviceKIFF(t, "t-eval", "viewer")
	var signed, said bytes.Buffer
	if err := peopleSignIn(context.Background(), srv.URL, "t-eval", "luis", key, srv.Client(), &signed, &said); err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(signed.String(), "\n")[0]; got != line {
		t.Fatal("signed in with another key than the one made")
	}
	signed.Reset()
	if err := peopleSignIn(context.Background(), srv.URL, "t-eval", "luis", nil, srv.Client(), &signed, &said); err == nil || signed.Len() != 0 {
		t.Fatalf("no key: err %v, wrote %q", err, signed.String())
	}
}
