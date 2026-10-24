package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	runs "github.com/kiff/agents/run"
)

// The owner answers a held call on the phone, as the tafs app does (kiff/tafs
// #31): Face ID signs KIFF's kiff.answer.v1 message over this hold, this
// answer and the hold's accounting version, with the phone's key, and the
// answer goes to KIFF with the signature. A session bound
// to a phone's key can't answer any other way.

// answerMessage is KIFF's signed answer message, kiff.answer.v1, one field per
// line (as kiff/tafs KIFFClient.answerMessage writes it).
// No next step and no note: the owner's answer only.
func answerMessage(tenant, exception, answer string, version int64, signedAt string) []byte {
	return []byte(strings.Join([]string{"kiff.answer.v1", tenant, exception, answer, "", "", strconv.FormatInt(version, 10), signedAt}, "\n"))
}

// signAnswer is the phone's signature over the answer: base64 DER ECDSA over
// the message's SHA-256, as the app's Secure Enclave key makes it.
func signAnswer(key *ecdsa.PrivateKey, msg []byte) (string, error) {
	digest := sha256.Sum256(msg)
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// answerHold answers KIFF's hold exception with answer, "approve-once" or
// "reject", signed by the phone. It first checks that the same answer,
// unsigned, is refused: the signature is what makes it the owner's.
func (t *test) answerHold(ctx context.Context, exception, answer string) error {
	path := "/v1/me/card-exceptions/" + url.PathEscape(exception)
	e, err := t.kiff.do(ctx, http.MethodGet, path, nil)
	if err != nil || e.err() != nil {
		return fmt.Errorf("reading the held call: %v %v", err, e.err())
	}
	if status, _ := e.Body["status"].(string); status != "held" {
		return fmt.Errorf("the held call is %q, not held", status)
	}
	version, ok := e.Body["accounting_version"].(float64)
	if !ok {
		return fmt.Errorf("the held call has no accounting_version")
	}

	unsigned, err := t.kiff.do(ctx, http.MethodPost, path+"/"+answer, map[string]any{})
	if err != nil {
		return err
	}
	if unsigned.Status != http.StatusForbidden || !strings.Contains(unsigned.Raw, "signature_required") {
		return fmt.Errorf("an unsigned %s was answered %d %s; want 403 signature_required", answer, unsigned.Status, oneLine(unsigned.Raw))
	}

	signedAt := time.Now().UTC().Format(time.RFC3339)
	sig, err := signAnswer(t.pk, answerMessage(t.tenant, exception, answer, int64(version), signedAt))
	if err != nil {
		return err
	}
	a, err := t.kiff.do(ctx, http.MethodPost, path+"/"+answer, map[string]any{"signature": map[string]string{"signed_at": signedAt, "value": sig}})
	if err != nil || a.err() != nil {
		return fmt.Errorf("the phone's signed %s: %v %v", answer, err, a.err())
	}
	return nil
}

// heldThenAnswered sends ask to the taf, expects KIFF to hold its refund,
// answers the hold on the phone with answer, records it on the paused run
// as kiff/agents' alerts would, and continues the run. It returns the run as
// it ended and the hold's exception.
func (t *test) heldThenAnswered(ctx context.Context, send func(string) (runs.Run, error), ask, answer string) (runs.Run, string, error) {
	r, err := send(ask)
	if err != nil {
		return r, "", err
	}
	if r.Status != runs.Paused || len(r.Requirements) != 1 || r.Requirements[0].Exception == "" {
		return r, "", fmt.Errorf("%q: the run ended %s with %d held calls; want it paused on one hold", ask, r.Status, len(r.Requirements))
	}
	exception := r.Requirements[0].Exception
	t.out.hide(exception, "<a hold>")
	if err := t.answerHold(ctx, exception, answer); err != nil {
		return r, exception, err
	}
	outcome := map[string]string{"approve-once": "approved", "reject": "refused"}[answer]
	if _, err := runs.Answer(ctx, t.rn.Runs, t.tenant, exception, outcome, time.Now()); err != nil {
		return r, exception, fmt.Errorf("recording the answer on the run: %w", err)
	}
	r, err = t.rn.Execute(ctx, t.tenant, r.ID, nil)
	if err != nil {
		return r, exception, fmt.Errorf("continuing the run: %w", err)
	}
	return r, exception, nil
}
