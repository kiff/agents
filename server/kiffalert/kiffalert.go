// Package kiffalert receives KIFF's hold_answered alerts: the owner answered
// a call KIFF held for a run (approved or refused), its hold expired, or it
// was withdrawn. The handler checks KIFF's signature, records the answer on
// the run that paused on that hold, and hands a run that no longer waits for
// anything to Continue, which executes it in the background. Without
// Continue, the answer is only recorded: the run is due ResolvedGrace later,
// and the sweep continues it (run.Sweeper.Continue).
//
// KIFF delivers an alert again until it gets a 2xx, so the status says
// whether it should: 200 once the answer is recorded (or there was nothing to
// record it on), 404 while no run has paused on the hold yet and the answer
// is recent (PauseWindow), 401 for an alert that doesn't check, 503 when
// continuing the run failed.
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

// PauseWindow is how long after the owner decided an alert for a hold no
// run has paused on is 404, so KIFF delivers it again: the owner may answer
// before the run's pause is stored, which takes seconds. Past it the hold is
// not ours (another client on the KIFF account, or a removed agent's run),
// and the alert is 200: KIFF switches off a target that keeps failing.
const PauseWindow = 10 * time.Minute

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
	// repeated alert); the run is claimed once. Nil leaves it to the sweep,
	// so the handler needs no power to execute anything.
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
	// DecidedAt is when KIFF resolved the hold, RFC 3339.
	DecidedAt string `json:"decided_at"`
	Hold      *struct {
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
		// The same answer as a bad signature: an unsigned post can't learn
		// which KIFF accounts have a tenant here.
		h.log().Info("kiffalert: an alert from an unknown KIFF account")
		http.Error(w, "the alert doesn't check", http.StatusUnauthorized)
		return
	case err != nil:
		h.log().Error("kiffalert: reading the account", "err", err)
		http.Error(w, "try again", http.StatusServiceUnavailable)
		return
	}
	if !Verify(secret, r.Header.Get(SignatureHeader), body, h.now()) {
		h.log().Info("kiffalert: an alert whose signature doesn't check", "tenant", tenant)
		http.Error(w, "the alert doesn't check", http.StatusUnauthorized)
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
	case errors.Is(err, run.ErrNotFound) && h.recent(a.DecidedAt):
		http.Error(w, "no run has paused on this hold yet", http.StatusNotFound)
		return
	case errors.Is(err, run.ErrNotFound):
		h.log().Info("kiffalert: no run paused on the hold", "tenant", tenant, "hold", a.Hold.ID)
		w.WriteHeader(http.StatusOK)
		return
	case err != nil:
		h.log().Error("kiffalert: recording the answer", "hold", a.Hold.ID, "err", err)
		http.Error(w, "try again", http.StatusServiceUnavailable)
		return
	}
	if !ready(rn, h.now()) {
		w.WriteHeader(http.StatusOK)
		return
	}
	if h.Continue != nil {
		if err := h.Continue(ctx, tenant, rn.ID); err != nil {
			// The answer is recorded; a later delivery continues the run.
			h.log().Error("kiffalert: continuing the run", "run", rn.ID, "err", err)
			http.Error(w, "the answer is recorded; continuing failed, try again", http.StatusServiceUnavailable)
			return
		}
	}
	// The run waits for nothing more. Its tenant's caller (tafs cloud, which
	// handed this alert on) may continue it now through the API, where it
	// is a caller, rather than wait for the sweep (kiff/tafs#138). Only an
	// alert signed with the account's secret gets this far.
	writeJSON(w, Ready{Run: ReadyRun{ID: rn.ID, Agent: rn.Agent}})
}

// Ready is the answer to an alert that left its run waiting for nothing
// more: the run, which its tenant's caller may continue
// (POST /agents/{agent}/runs/{id}/continue).
type Ready struct {
	Run ReadyRun `json:"run"`
}

// ReadyRun names a run that may continue.
type ReadyRun struct {
	ID    string `json:"id"`
	Agent string `json:"agent"`
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

// recent reports whether KIFF decided the hold within PauseWindow of now. A
// missing or unreadable time is not recent.
func (h *Handler) recent(decidedAt string) bool {
	at, err := time.Parse(time.RFC3339Nano, decidedAt)
	return err == nil && h.now().Sub(at) < PauseWindow
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
