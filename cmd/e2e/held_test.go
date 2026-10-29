package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/kiff/agents/session"
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

// The refused half of the R-3005 test reads the run's Called record, which
// stays after the run ends; its ToolCall events are gone by then. A run that
// truly skipped the look-up fails it, and so does one whose only record of
// the look-up is a ToolCall event.
func TestTheLookUpCheckReadsCalledAndCanFail(t *testing.T) {
	ev := func(run string, kind session.EventKind, body string) session.Event {
		return session.Event{Run: run, Kind: kind, Body: json.RawMessage(body)}
	}
	reads := []string{"retrieve_charge_ab12"} // the grant's name for its charge read
	for _, c := range []struct {
		what   string
		events []session.Event
		want   bool
	}{
		{"looked up, then refused", []session.Event{
			ev("r1", session.Called, `{"tool":"stripe_retrieve_charge_ab12","outcome":"done"}`),
			ev("r1", session.Called, `{"tool":"stripe_create_refund_ab12","outcome":"held"}`),
		}, true},
		{"skipped the look-up", []session.Event{
			ev("r1", session.Called, `{"tool":"stripe_create_refund_ab12","outcome":"held"}`),
		}, false},
		{"the look-up only as a ToolCall", []session.Event{
			ev("r1", session.ToolCall, `{"tool":"stripe_retrieve_charge_ab12"}`),
			ev("r1", session.Called, `{"tool":"stripe_create_refund_ab12","outcome":"held"}`),
		}, false},
		{"looked up in another run", []session.Event{
			ev("r0", session.Called, `{"tool":"stripe_retrieve_charge_ab12","outcome":"done"}`),
			ev("r1", session.Called, `{"tool":"stripe_create_refund_ab12","outcome":"held"}`),
		}, false},
	} {
		tools, err := calledIn(c.events, "r1")
		if err != nil {
			t.Fatal(err)
		}
		if got := lookedUp(tools, reads); got != c.want {
			t.Errorf("%s: looked up %v, want %v (calls %v)", c.what, got, c.want, tools)
		}
	}
}
