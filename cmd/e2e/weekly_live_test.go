package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/cmd/internal/testaccount"
)

func TestTheReportIsSignedAsASetupCallKIFFChecks(t *testing.T) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"result":"connected","session":"s1"}`)
	req := httptest.NewRequest("POST", "https://cloud.example/v1/tafs/coach/components/c1/approval/a1", nil)
	now := time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)
	h := deviceSignature(k, "t1", req, body, now)
	at, sig, ok := strings.Cut(strings.TrimPrefix(h, "t="), ",v1=")
	if !ok || at != "2026-11-18T06:00:00Z" {
		t.Fatalf("header %q", h)
	}
	sum := sha256.Sum256(body)
	msg := "kiff.request.v1\nt1\nPOST\n/v1/tafs/coach/components/c1/approval/a1\n" + hex.EncodeToString(sum[:]) + "\n" + at
	digest := sha256.Sum256([]byte(msg))
	raw, _ := base64.StdEncoding.DecodeString(sig)
	if !ecdsa.VerifyASN1(&k.PublicKey, digest[:], raw) {
		t.Fatal("the signature doesn't verify over kiff.request.v1")
	}
}

func TestAGrantToolIsReadByItsCatalogName(t *testing.T) {
	for name, want := range map[string]string{
		"pipedrive_list_persons_x9k2": "pipedrive_list_persons",
		"pipedrive_list_persons":      "pipedrive_list_persons",
		"pipedrive_list_all":          "pipedrive_list_all",
	} {
		if got := catalogName(testaccount.GrantTool{Name: name}); got != want {
			t.Errorf("catalogName(%q) = %q, want %q", name, got, want)
		}
	}
}
