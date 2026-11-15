package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kiff/agents/cmd/internal/testaccount"
)

// fakeCloud is tafs cloud's check route, planque's grants list and KIFF's
// draft confirm, as the harness calls them, keeping each call made.
type fakeCloud struct {
	mu      sync.Mutex
	key     *ecdsa.PrivateKey
	planque string
	answer  map[string]any // what the check route answers
	calls   []string
	asked   map[string]string // the check's body
	confirm map[string]string // KIFF's confirm body
	proofOK bool
}

func (f *fakeCloud) servers(t *testing.T) (cloud, planque, kiff *httptest.Server) {
	planque = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"grants": []map[string]any{
			{"id": "grt_other", "status": "connected", "request": map[string]any{"agent": "someone-else"}},
			{"id": "grt_1", "status": "connected", "request": map[string]any{"agent": "refunds-x"}},
		}})
	}))
	f.planque = planque.URL
	cloud = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, "cloud "+r.Method+" "+r.URL.Path)
		_ = json.NewDecoder(r.Body).Decode(&f.asked)
		f.proofOK = r.Header.Get("Authorization") == "Bearer session" && f.verifies(r.Header.Get("Planque-Device-Signature"))
		_ = json.NewEncoder(w).Encode(f.answer)
	}))
	kiff = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, "kiff "+r.Method+" "+r.URL.Path)
		_ = json.NewDecoder(r.Body).Decode(&f.confirm)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"version":"v3","agent_id":"refunds-x","status":"draft"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"confirmed","agent_id":"refunds-x"}`))
	}))
	t.Cleanup(planque.Close)
	t.Cleanup(cloud.Close)
	t.Cleanup(kiff.Close)
	return cloud, planque, kiff
}

// verifies is planque's own check of the proof tafs cloud forwards: the
// phone's signature over its read of the account's grants, with no body.
func (f *fakeCloud) verifies(h string) bool {
	var at, sig string
	for _, part := range strings.Split(h, ",") {
		if k, v, ok := strings.Cut(part, "="); ok {
			switch k {
			case "t":
				at = v
			case "v1":
				sig = v
			}
		}
	}
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil || at == "" {
		return false
	}
	empty := sha256.Sum256(nil)
	msg := strings.Join([]string{"planque.request.v1", f.planque, "tnt", http.MethodGet, "/v1/accounts/tnt/grants", hex.EncodeToString(empty[:]), at}, "\n")
	digest := sha256.Sum256([]byte(msg))
	return ecdsa.VerifyASN1(&f.key.PublicKey, digest[:], raw)
}

func checkWorld(t *testing.T, answer map[string]any) (*test, *fakeCloud) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	f := &fakeCloud{key: key, answer: answer}
	cloud, planque, kiff := f.servers(t)
	out := &redacting{w: &strings.Builder{}}
	tt := &test{out: out, cfg: config{cloud: cloud.URL, session: "session", log: out}, hc: cloud.Client(),
		phone: testaccount.NewPhone(key, planque.URL, "tnt", planque.Client()),
		kiff:  &client{Base: kiff.URL, HTTP: kiff.Client()}, grants: []string{"grt_other", "grt_1"}}
	return tt, f
}

// Eval 43681227913: the harness confirmed the draft in KIFF itself, so none of
// Approve's checks ran on it. Now tafs cloud's check runs first, with the
// phone's own signed read of its grants and the owner's session, and a
// refused draft is never confirmed.
func TestTafsCloudChecksAVersionsDraftBeforeKIFFConfirmsIt(t *testing.T) {
	refused := map[string]any{"outcome": "answered", "code": "vendor_name",
		"answer": `failed: the Card covers "list_charges", but this taf's connection gives that tool as "list_charges_noyy"`}
	tt, f := checkWorld(t, refused)
	draft := map[string]any{"version": "v3", "agent_id": "refunds-x"}
	_, err := tt.confirmChecked(context.Background(), "drf_1", draft, "trial")
	if err == nil || !strings.Contains(err.Error(), "(vendor_name)") || !strings.Contains(err.Error(), "list_charges_noyy") || !strings.Contains(err.Error(), checkRoute) {
		t.Fatalf("a refused version's Card: %v", err)
	}
	// Checked for the taf's own grant, at the version read, with a proof planque
	// takes; and nothing confirmed.
	if strings.Join(f.calls, "\n") != "cloud POST /v1/connect/grants/grt_1/check" || !f.proofOK ||
		f.asked["draft"] != "drf_1" || f.asked["shown_draft"] != "v3" {
		t.Fatalf("calls %v, proof %v, asked %v", f.calls, f.proofOK, f.asked)
	}
	// Passed: confirmed in KIFF at the version the check passed, as a trial when asked.
	f.answer, f.calls = map[string]any{"outcome": "ok", "version": "v3"}, nil
	a, err := tt.confirmChecked(context.Background(), "drf_1", draft, "trial")
	if err != nil || a.Err() != nil {
		t.Fatal(err, a.Err())
	}
	if strings.Join(f.calls, "\n") != "cloud POST /v1/connect/grants/grt_1/check\nkiff POST /v1/me/agent-drafts/drf_1/confirm" ||
		f.confirm["version"] != "v3" || f.confirm["as"] != "trial" {
		t.Fatalf("calls %v, confirm %v", f.calls, f.confirm)
	}
	// A draft for a taf none of this run's connected grants is for: not checked against another's grant.
	if _, err := tt.confirmChecked(context.Background(), "drf_1", map[string]any{"version": "v3", "agent_id": "nobody"}, ""); err == nil {
		t.Fatal("a draft with no grant of its taf was checked")
	}
}

// The check's answers as the harness reads them: ok with its version, a
// refusal to hand Coach, and anything else a failed step, never a confirm.
func TestTheChecksAnswersAreRead(t *testing.T) {
	for _, c := range []struct {
		body map[string]any
		ok   bool
		err  bool
	}{
		{map[string]any{"outcome": "ok", "version": "v3"}, true, false},
		{map[string]any{"outcome": "ok"}, false, true},
		{map[string]any{"outcome": "answered", "answer": "failed: no Card covers \"Read a charge\"", "code": "uncovered"}, false, false},
		{map[string]any{"outcome": "answered", "answer": "approved"}, false, true},
		{map[string]any{"outcome": "read_again", "code": "cards_unread"}, false, true},
		{map[string]any{"outcome": "sign"}, false, true},
		{map[string]any{}, false, true},
	} {
		got, err := checkedFrom(c.body)
		if (err != nil) != c.err || got.OK != c.ok {
			t.Errorf("%v: %+v, %v", c.body, got, err)
		}
		if !c.err && !c.ok && (got.Answer == "" || got.Code == "") {
			t.Errorf("%v: a refusal without its answer or code: %+v", c.body, got)
		}
	}
}

// Each call of the check carries a new proof: planque takes a signed read once.
func TestEachCheckSignsItsOwnRead(t *testing.T) {
	tt, _ := checkWorld(t, map[string]any{"outcome": "ok", "version": "v3"})
	a, err := tt.phone.GrantsProof(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := tt.phone.GrantsProof(context.Background())
	if a == "" || a == b {
		t.Fatalf("proofs %q and %q", a, b)
	}
}

// The setup chat's approval: a draft tafs cloud's check refuses isn't
// confirmed, and its answer is what the card's answer to Coach carries, so
// Coach drafts again as it would on an owner's app; one it passes is
// confirmed in KIFF at the version checked.
func TestARefusedDraftGoesBackToCoachUnconfirmed(t *testing.T) {
	refused := map[string]any{"outcome": "answered", "code": "vendor_name", "answer": `failed: the Card covers "list_charges", but …`}
	tt, f := checkWorld(t, refused)
	tt.cfg.transcript = &strings.Builder{}
	c := component{Type: "approval", ID: "a1", Grant: "grt_1", Draft: "drf_1"}
	var approved string
	reread, answer, err := tt.approve(context.Background(), c, &approved)
	if err != nil || reread || answer != refused["answer"] {
		t.Fatalf("reread %v, answer %q, err %v", reread, answer, err)
	}
	if got := refusedAnswer(c, answer); got != `[approval a1] failed: the Card covers "list_charges", but …` {
		t.Fatalf("Coach hears %q", got)
	}
	if strings.Join(f.calls, "\n") != "kiff GET /v1/me/agent-drafts/drf_1\ncloud POST /v1/connect/grants/grt_1/check" ||
		len(tt.draftIDs) != 0 || strings.Join(tt.draftRefusals, ",") != "vendor_name" {
		t.Fatalf("calls %v, confirmed %v, refusals %v", f.calls, tt.draftIDs, tt.draftRefusals)
	}
	// The public log says how many drafts were refused, by code, and none when none were.
	if got := refusalsLine(tt.draftRefusals); got != "setup drafts refused by tafs cloud's check: 1 (vendor_name)" {
		t.Fatalf("the log says %q", got)
	}
	if got := refusalsLine(nil); got != "setup drafts refused by tafs cloud's check: 0" {
		t.Fatalf("with none, the log says %q", got)
	}
	if got := refusalsLine([]string{"vendor_name", "uncovered"}); got != "setup drafts refused by tafs cloud's check: 2 (vendor_name, uncovered)" {
		t.Fatalf("two: %q", got)
	}
	// The transcript names the route that refused it, and its code.
	if tr := tt.cfg.transcript.(*strings.Builder).String(); !strings.Contains(tr, checkRoute) || !strings.Contains(tr, "vendor_name") {
		t.Fatalf("transcript: %s", tr)
	}
	// Coach's next draft passes: confirmed in KIFF, at the version checked.
	f.answer, f.calls = map[string]any{"outcome": "ok", "version": "v3"}, nil
	c.ID, c.Draft = "a2", "drf_2"
	if _, answer, err = tt.approve(context.Background(), c, &approved); err != nil || answer != "" {
		t.Fatalf("answer %q, err %v", answer, err)
	}
	if strings.Join(f.calls, "\n") != "kiff GET /v1/me/agent-drafts/drf_2\ncloud POST /v1/connect/grants/grt_1/check\nkiff POST /v1/me/agent-drafts/drf_2/confirm" ||
		f.confirm["version"] != "v3" || strings.Join(tt.draftIDs, ",") != "drf_2" {
		t.Fatalf("calls %v, confirm %v, confirmed %v", f.calls, f.confirm, tt.draftIDs)
	}
	// A check that fails (tafs cloud says read again): the step fails, nothing confirmed.
	f.answer, f.calls = map[string]any{"outcome": "read_again", "code": "cards_unread"}, nil
	c.ID, c.Draft = "a3", "drf_3"
	if _, _, err = tt.approve(context.Background(), c, &approved); err == nil || !strings.Contains(err.Error(), "cards_unread") {
		t.Fatalf("a read again: %v", err)
	}
	if strings.Contains(strings.Join(f.calls, "\n"), "confirm") {
		t.Fatalf("confirmed after a read again: %v", f.calls)
	}
}
