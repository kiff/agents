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

// fakeDeviceKIFF is KIFF's device sign-in, approved on the second ask, and
// /v1/me answering role for the session it gave.
func fakeDeviceKIFF(t *testing.T, role string) *httptest.Server {
	t.Helper()
	asks := 0
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
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "sess-luis", "tenant": "t-eval"})
		case "/v1/me":
			if r.Header.Get("Authorization") != "Bearer sess-luis" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"tenant_display_name": "KIFF Eval", "member_role": role})
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
	srv := fakeDeviceKIFF(t, "viewer")
	var out, say bytes.Buffer
	if err := peopleSignIn(context.Background(), srv.URL, "luis", srv.Client(), &out, &say); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 || lines[1] != "sess-luis" {
		t.Fatalf("wrote %q", out.String())
	}
	if _, err := parsePhone(lines[0]); err != nil {
		t.Fatalf("the phone key doesn't parse: %v", err)
	}
	if s := say.String(); strings.Contains(s, "sess-luis") || strings.Contains(s, lines[0]) || !strings.Contains(s, `"KIFF Eval" as viewer`) {
		t.Fatalf("printed %q", s)
	}
}

// Approved on another account, or with another role, nothing is written.
func TestAPersonOnTheWrongAccountWritesNothing(t *testing.T) {
	devicePoll = time.Millisecond
	srv := fakeDeviceKIFF(t, "admin")
	var out, say bytes.Buffer
	err := peopleSignIn(context.Background(), srv.URL, "luis", srv.Client(), &out, &say)
	if err == nil || !strings.Contains(err.Error(), "not as viewer") || out.Len() != 0 {
		t.Fatalf("err %v, wrote %q", err, out.String())
	}
	if err := peopleSignIn(context.Background(), srv.URL, "ana", srv.Client(), &out, &say); err == nil || out.Len() != 0 {
		t.Fatalf("an unknown person: err %v, wrote %q", err, out.String())
	}
}
