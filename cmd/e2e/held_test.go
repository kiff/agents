package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

// The owner's answer is signed as the tafs app signs it (kiff/tafs
// KIFFClient.answerMessage) and as KIFF checks it (auth.VerifyAnswer): the
// eight lines of kiff.answer.v1, no next step or note, base64 DER ECDSA over
// their SHA-256 with the phone's P-256 key.
func TestTheOwnersAnswerIsSignedAsTheAppSignsIt(t *testing.T) {
	msg := answerMessage("t_01", "exc_9", "approve-once", 3, "2026-10-18T14:00:00Z")
	if want := "kiff.answer.v1\nt_01\nexc_9\napprove-once\n\n\n3\n2026-10-18T14:00:00Z"; string(msg) != want {
		t.Fatalf("message %q, want %q", msg, want)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := signAnswer(key, msg)
	if err != nil {
		t.Fatal(err)
	}
	der, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(msg)
	if !ecdsa.VerifyASN1(&key.PublicKey, digest[:], der) {
		t.Fatal("KIFF's check would refuse the signature")
	}
	other := sha256.Sum256(answerMessage("t_01", "exc_9", "reject", 3, "2026-10-18T14:00:00Z"))
	if ecdsa.VerifyASN1(&key.PublicKey, other[:], der) {
		t.Fatal("an approval's signature verifies as a rejection")
	}
}
