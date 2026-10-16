package kiffalert_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/server/kiffalert"
	"github.com/kiff/agents/store/memory"
)

const secret = "whsec_test"

var ctx = context.Background()

type accounts struct{ err error }

func (a accounts) Account(_ context.Context, kiffAccount string) (string, string, error) {
	if a.err != nil {
		return "", "", a.err
	}
	if kiffAccount == "kiff-acct-blank" {
		return "t1", "", nil // a target KIFF sends unsigned: it has no secret
	}
	if kiffAccount != "kiff-acct-1" {
		return "", "", kiffalert.ErrUnknownAccount
	}
	return "t1", secret, nil
}

type env struct {
	h         *kiffalert.Handler
	runs      *memory.Runs
	now       time.Time
	continued []string
	run       run.Run
}

// setup is a handler on a memory store holding t1's run paused on two calls
// KIFF holds, exc_1 and exc_2.
func setup(t *testing.T) *env {
	t.Helper()
	e := &env{runs: memory.NewRuns(), now: time.Now().UTC().Truncate(time.Second)}
	r := run.New("t1", "rae", "s1", run.ByOwner, "Refund two orders.", e.now)
	if err := e.runs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	r, err := run.Claim(ctx, e.runs, "t1", r.ID, "i1", e.now.Add(time.Minute), e.now)
	if err != nil {
		t.Fatal(err)
	}
	e.run, err = run.Save(ctx, e.runs, r, e.now, func(n *run.Run) error {
		n.Status = run.Paused
		for _, ex := range []string{"exc_1", "exc_2"} {
			n.Requirements = append(n.Requirements, run.Requirement{CallID: "c-" + ex, Tool: "stripe_refund", Operation: "op-" + ex,
				Exception: ex, Expires: e.now.Add(10 * time.Minute)})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.h = &kiffalert.Handler{Accounts: accounts{}, Runs: e.runs, Now: func() time.Time { return e.now },
		Continue: func(_ context.Context, tenant, id string) error {
			e.continued = append(e.continued, tenant+"/"+id)
			return nil
		}}
	return e
}

// payload is a hold_answered alert as KIFF's dispatcher sends it.
func payload(account, outcome, hold, status string) string {
	return fmt.Sprintf(`{"schema_version":"1","type":"decision","tenant_id":%q,"action":"stripe.refund","entity_id":"ch_big",`+
		`"outcome":%q,"hold":{"id":%q,"status":%q},"decided_at":"2026-10-16T09:03:12Z","event_id":"evt_1"}`, account, outcome, hold, status)
}

// signed is KIFF's KIFF-Signature for body at t.
func signed(body string, t time.Time) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return "t=" + ts + ",v1=" + hex.EncodeToString(kiffalert.Sign(secret, ts, []byte(body)))
}

func (e *env) post(body, signature string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/kiff/alerts", strings.NewReader(body))
	if signature != "" {
		req.Header.Set(kiffalert.SignatureHeader, signature)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	return w
}

func (e *env) stored(t *testing.T) run.Run {
	t.Helper()
	r, err := e.runs.Get(ctx, "t1", e.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Each answer is recorded on its requirement; the run continues once both
// are answered, not before. A repeated alert is 200 and records nothing new.
func TestAnswersAreRecordedAndTheRunContinuesOnceAllAre(t *testing.T) {
	e := setup(t)
	body := payload("kiff-acct-1", kiffalert.HoldAnswered, "exc_1", "approved")
	if w := e.post(body, signed(body, e.now)); w.Code != http.StatusOK {
		t.Fatalf("first answer: %d %s", w.Code, w.Body)
	}
	if q := e.stored(t).Requirements[0]; q.Answer != "approved" || q.Answered.IsZero() || len(e.continued) != 0 {
		t.Fatalf("after one answer: %+v, continued %v", q, e.continued)
	}
	body = payload("kiff-acct-1", kiffalert.HoldAnswered, "exc_2", "refused")
	if w := e.post(body, signed(body, e.now)); w.Code != http.StatusOK {
		t.Fatalf("second answer: %d %s", w.Code, w.Body)
	}
	if q := e.stored(t).Requirements[1]; q.Answer != "refused" {
		t.Fatalf("second requirement %+v", q)
	}
	if len(e.continued) != 1 || e.continued[0] != "t1/"+e.run.ID {
		t.Fatalf("continued %v, want the run once", e.continued)
	}
	before := e.stored(t)
	if w := e.post(body, signed(body, e.now)); w.Code != http.StatusOK {
		t.Fatalf("a repeat: %d", w.Code)
	}
	if after := e.stored(t); after.Version != before.Version {
		t.Fatal("a repeat changed the run")
	}
}

// What doesn't check is refused, and nothing is recorded: no signature, a
// wrong one, one made with another secret, one too old or from the future,
// and an account no tenant here takes alerts from.
func TestAnAlertThatDoesntCheckIsRefused(t *testing.T) {
	e := setup(t)
	body := payload("kiff-acct-1", kiffalert.HoldAnswered, "exc_1", "approved")
	ts := strconv.FormatInt(e.now.Unix(), 10)
	other := "t=" + ts + ",v1=" + hex.EncodeToString(kiffalert.Sign("whsec_other", ts, []byte(body)))
	stranger := payload("kiff-acct-2", kiffalert.HoldAnswered, "exc_1", "approved")
	for name, c := range map[string]struct{ body, sig string }{
		"unsigned":        {body, ""},
		"garbled":         {body, "t=" + ts + ",v1=00"},
		"another secret":  {body, other},
		"too old":         {body, signed(body, e.now.Add(-kiffalert.Tolerance-time.Second))},
		"from the future": {body, signed(body, e.now.Add(kiffalert.Tolerance+time.Second))},
		"another body":    {body, signed(strings.Replace(body, "approved", "refused", 1), e.now)},
		"unknown account": {stranger, signed(stranger, e.now)},
	} {
		if w := e.post(c.body, c.sig); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: %d, want 401", name, w.Code)
		}
	}
	if q := e.stored(t).Requirements[0]; !q.Answered.IsZero() {
		t.Fatalf("recorded %+v", q)
	}
}

// A signature with two v1 values checks if either does (a secret being
// rotated), and one within the tolerance checks.
func TestASignatureMayCarrySeveralValues(t *testing.T) {
	e := setup(t)
	body := payload("kiff-acct-1", kiffalert.HoldAnswered, "exc_1", "approved")
	sig := signed(body, e.now.Add(-time.Minute))
	if w := e.post(body, sig[:strings.Index(sig, ",")]+",v1=deadbeef,"+sig[strings.Index(sig, ",")+1:]); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// An alert for a hold no run has paused on yet is 404, so KIFF delivers it
// again; one for a run that went on is 200, and continues nothing.
func TestTheStatusSaysWhetherKIFFShouldDeliverAgain(t *testing.T) {
	e := setup(t)
	body := payload("kiff-acct-1", kiffalert.HoldAnswered, "exc_9", "approved")
	if w := e.post(body, signed(body, e.now)); w.Code != http.StatusNotFound {
		t.Fatalf("an unknown hold: %d, want 404", w.Code)
	}
	r, err := run.Claim(ctx, e.runs, "t1", e.run.ID, "i2", e.now.Add(time.Hour), e.now.Add(11*time.Minute)) // both expired
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.Save(ctx, e.runs, r, e.now.Add(11*time.Minute), func(n *run.Run) error {
		n.Status, n.Requirements, n.Expires = run.Completed, nil, e.now.Add(time.Hour)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	body = payload("kiff-acct-1", kiffalert.HoldAnswered, "exc_1", "expired")
	if w := e.post(body, signed(body, e.now)); w.Code != http.StatusOK || len(e.continued) != 0 {
		t.Fatalf("an ended run: %d, continued %v", w.Code, e.continued)
	}
}

// Another outcome is acknowledged and ignored; a malformed alert is 400; the
// wrong method is 405.
func TestOtherRequests(t *testing.T) {
	e := setup(t)
	body := payload("kiff-acct-1", "owner_required", "exc_1", "held")
	if w := e.post(body, signed(body, e.now)); w.Code != http.StatusOK || !e.stored(t).Requirements[0].Answered.IsZero() {
		t.Fatalf("owner_required: %d", w.Code)
	}
	noHold := `{"tenant_id":"kiff-acct-1","outcome":"hold_answered"}`
	if w := e.post(noHold, signed(noHold, e.now)); w.Code != http.StatusBadRequest {
		t.Fatalf("no hold: %d, want 400", w.Code)
	}
	if w := e.post("not json", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("not json: %d, want 400", w.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/kiff/alerts", nil)
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET: %d", w.Code)
	}
	big := `{"tenant_id":"` + strings.Repeat("x", kiffalert.MaxBody) + `"}`
	if w := e.post(big, ""); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a large body: %d, want 413", w.Code)
	}
}

// When continuing the run fails, the answer stays recorded and the alert is
// 503, so KIFF delivers it again and the run is continued then. A failure to
// read the account is 503 as well.
func TestAFailedContinueIsDeliveredAgain(t *testing.T) {
	e := setup(t)
	fail := true
	e.h.Continue = func(_ context.Context, tenant, id string) error {
		if fail {
			return errors.New("invoke failed")
		}
		e.continued = append(e.continued, tenant+"/"+id)
		return nil
	}
	for _, ex := range []string{"exc_1", "exc_2"} {
		body := payload("kiff-acct-1", kiffalert.HoldAnswered, ex, "approved")
		e.post(body, signed(body, e.now))
	}
	body := payload("kiff-acct-1", kiffalert.HoldAnswered, "exc_2", "approved")
	if w := e.post(body, signed(body, e.now)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("a failed continue: %d, want 503", w.Code)
	}
	fail = false
	if w := e.post(body, signed(body, e.now)); w.Code != http.StatusOK || len(e.continued) != 1 {
		t.Fatalf("delivered again: %d, continued %v", w.Code, e.continued)
	}
	e.h.Accounts = accounts{err: errors.New("store down")}
	if w := e.post(body, signed(body, e.now)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("account read failed: %d, want 503", w.Code)
	}
}

// A signature made by KIFF's own alert.SignatureHeader (kiff-cloud,
// apps/api/internal/alert) checks: the scheme is KIFF's, byte for byte.
func TestKIFFsOwnSignatureChecks(t *testing.T) {
	body := []byte(`{"tenant_id":"kiff-acct-1","outcome":"hold_answered","hold":{"id":"exc_1","status":"approved"}}`)
	header := "t=1760605800,v1=626c49dffc5af165d8bf32513062d95528a2cf37b7904511c5e5bdedaa803211"
	if !kiffalert.Verify("whsec_test", header, body, time.Unix(1760605800+60, 0)) {
		t.Fatal("KIFF's signature doesn't check")
	}
	if kiffalert.Verify("whsec_test", header, append(body, ' '), time.Unix(1760605800, 0)) {
		t.Fatal("a changed body checks")
	}
}

// An account with no signing secret is refused, even for an alert "signed"
// with the empty key: KIFF sends a target made before signing unsigned, and
// an unsigned alert proves nothing.
func TestAnAccountWithNoSecretIsRefused(t *testing.T) {
	e := setup(t)
	body := payload("kiff-acct-blank", kiffalert.HoldAnswered, "exc_1", "approved")
	ts := strconv.FormatInt(e.now.Unix(), 10)
	blank := "t=" + ts + ",v1=" + hex.EncodeToString(kiffalert.Sign("", ts, []byte(body)))
	if w := e.post(body, blank); w.Code != http.StatusUnauthorized {
		t.Fatalf("%d, want 401", w.Code)
	}
}
