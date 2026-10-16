// Package kiffalert receives KIFF's hold_answered alerts: the owner answered
// a call KIFF held for a run (approved or refused), its hold expired, or it
// was withdrawn. The handler checks KIFF's signature, records the answer on
// the run that paused on that hold, and hands a run that no longer waits for
// anything to Continue, which executes it in the background.
//
// KIFF delivers an alert again until it gets a 2xx, so the status says
// whether it should: 200 once the answer is recorded (or there was nothing to
// record it on), 404 while no run has paused on the hold yet, 401 for a
// signature that doesn't check, 503 when continuing the run failed.
package kiffalert

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kiff/agents/run"
)

// HoldAnswered is the alert's outcome for an answered, expired or withdrawn
// hold. Other outcomes are acknowledged and ignored.
const HoldAnswered = "hold_answered"

// SignatureHeader carries "t=<unix seconds>,v1=<hex HMAC-SHA256(secret,
// "<t>.<body>")>".
const SignatureHeader = "KIFF-Signature"

// Tolerance is how far an alert's signed time may be from now.
const Tolerance = 5 * time.Minute

// MaxBody bounds an alert's body.
const MaxBody = 64 << 10

// ErrUnknownAccount means no tenant here takes alerts from that KIFF account.
var ErrUnknownAccount = errors.New("kiffalert: unknown KIFF account")

// Accounts resolves the KIFF account an alert names to the tenant here and
// the signing secret of the alert target KIFF sends it through.
type Accounts interface {
	Account(ctx context.Context, kiffAccount string) (tenant, secret string, err error)
}

// Handler receives KIFF's alerts.
type Handler struct {
	Accounts Accounts
	Runs     run.Runs
	// Continue executes the tenant's run in the background: every call it
	// waited for is resolved. It may be called again for the same run (a
	// repeated alert); the run is claimed once.
	Continue func(ctx context.Context, tenant, runID string) error
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	Log *slog.Logger
}

// Alert is what the handler reads of KIFF's payload.
type Alert struct {
	TenantID string `json:"tenant_id"`
	Outcome  string `json:"outcome"`
	EventID  string `json:"event_id"`
	Hold     *struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"hold"`
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Handler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		http.Error(w, "the body is too large", http.StatusRequestEntityTooLarge)
		return
	}
	var a Alert
	if err := json.Unmarshal(body, &a); err != nil || a.TenantID == "" {
		http.Error(w, "not a KIFF alert", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	tenant, secret, err := h.Accounts.Account(ctx, a.TenantID)
	switch {
	case errors.Is(err, ErrUnknownAccount):
		http.Error(w, "unknown account", http.StatusUnauthorized)
		return
	case err != nil:
		h.log().Error("kiffalert: reading the account", "err", err)
		http.Error(w, "try again", http.StatusServiceUnavailable)
		return
	}
	if !Verify(secret, r.Header.Get(SignatureHeader), body, h.now()) {
		http.Error(w, "the signature doesn't check", http.StatusUnauthorized)
		return
	}
	if a.Outcome != HoldAnswered {
		w.WriteHeader(http.StatusOK) // not ours to act on; don't have it sent again
		return
	}
	if a.Hold == nil || a.Hold.ID == "" || a.Hold.Status == "" {
		http.Error(w, "a hold_answered alert names its hold and status", http.StatusBadRequest)
		return
	}
	rn, err := run.Answer(ctx, h.Runs, tenant, a.Hold.ID, a.Hold.Status, h.now())
	switch {
	case errors.Is(err, run.ErrNotFound):
		http.Error(w, "no run has paused on this hold yet", http.StatusNotFound)
		return
	case err != nil:
		h.log().Error("kiffalert: recording the answer", "hold", a.Hold.ID, "err", err)
		http.Error(w, "try again", http.StatusServiceUnavailable)
		return
	}
	if ready(rn, h.now()) {
		if err := h.Continue(ctx, tenant, rn.ID); err != nil {
			// The answer is recorded; a later delivery continues the run.
			h.log().Error("kiffalert: continuing the run", "run", rn.ID, "err", err)
			http.Error(w, "the answer is recorded; continuing failed, try again", http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

// ready reports whether r is paused and waits for nothing more.
func ready(r run.Run, now time.Time) bool {
	if r.Status != run.Paused || len(r.Requirements) == 0 {
		return false
	}
	for _, q := range r.Requirements {
		if !q.Resolved(now) {
			return false
		}
	}
	return true
}

// Verify reports whether header is KIFF's signature of body with secret,
// made within Tolerance of now. Any of several v1 values may match, so a
// rotated secret can overlap.
func Verify(secret, header string, body []byte, now time.Time) bool {
	if secret == "" || header == "" {
		return false
	}
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		switch {
		case !ok:
		case k == "t":
			ts = v
		case k == "v1":
			sigs = append(sigs, v)
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if d := now.Sub(time.Unix(sec, 0)); d > Tolerance || d < -Tolerance {
		return false
	}
	want := Sign(secret, ts, body)
	for _, s := range sigs {
		if got, err := hex.DecodeString(s); err == nil && hmac.Equal(got, want) {
			return true
		}
	}
	return false
}

// Sign is KIFF's signature of body at ts: HMAC-SHA256(secret, "<ts>.<body>").
func Sign(secret, ts string, body []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return mac.Sum(nil)
}
