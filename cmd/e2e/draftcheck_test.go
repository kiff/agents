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
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kiff/agents/cmd/internal/testaccount"
	"github.com/kiff/agents/tool/show"
)

// fakeCloud is tafs cloud's check route, planque's grants list and KIFF's
// draft confirm, as the harness calls them, keeping each call made.
type fakeCloud struct {
	mu      sync.Mutex
	key     *ecdsa.PrivateKey
	planque string
	answer  map[string]any // what the check route answers
	calls   []string
	asked   map[string]any // the check's body
	// onRead is what tafs cloud answers as checked_on_read, by grant, where
	// the answer doesn't say it: the cards that sign the Card.
	onRead map[string]bool
	confirm map[string]string // KIFF's confirm body
	proofOK bool
}

func (f *fakeCloud) servers(t *testing.T) (cloud, planque, kiff *httptest.Server) {
	planque = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"grants": []map[string]any{
			{"id": "grt_other", "status": "connected", "request": map[string]any{"agent": "someone-else"}},
			{"id": "grt_1", "status": "connected", "request": map[string]any{"agent": "refunds-x"}},
			{"id": "grt_listed", "status": "listed", "request": map[string]any{"agent": "refunds-x"}},
			{"id": "grt_adding", "status": "pending", "signed_in": true, "request": map[string]any{"agent": "refunds-x"}},
			{"id": "grt_key", "status": "pending", "request": map[string]any{"agent": "refunds-x", "kind": "api_key"}},
		}})
	}))
	f.planque = planque.URL
	cloud = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, "cloud "+r.Method+" "+r.URL.Path)
		_ = json.NewDecoder(r.Body).Decode(&f.asked)
		f.proofOK = r.Header.Get("Authorization") == "Bearer session" && f.verifies(r.Header.Get("Planque-Device-Signature"))
		answer := maps.Clone(f.answer)
		if _, said := answer["checked_on_read"]; !said && answer != nil && answer["outcome"] != "read_again" {
			grant := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/connect/grants/"), "/check")
			answer["checked_on_read"] = f.onRead[grant]
		}
		_ = json.NewEncoder(w).Encode(answer)
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
	// As tafs cloud answers for these grants: a connected app's Card alone, the
	// Card with added tools or a key are checked on the read; a listed one isn't.
	f := &fakeCloud{key: key, answer: answer, onRead: map[string]bool{"grt_1": true, "grt_adding": true, "grt_key": true}}
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
		{map[string]any{"outcome": "ok", "version": "v3", "checked_on_read": false}, true, false},
		{map[string]any{"outcome": "ok", "checked_on_read": true}, false, true},
		{map[string]any{"outcome": "answered", "answer": "failed: no Card covers \"Read a charge\"", "code": "uncovered", "checked_on_read": true}, false, false},
		{map[string]any{"outcome": "answered", "answer": "approved", "checked_on_read": true}, false, true},
		// A tafs cloud that doesn't say checked_on_read: the harness won't guess.
		{map[string]any{"outcome": "ok", "version": "v3"}, false, true},
		{map[string]any{"outcome": "answered", "answer": "failed: no Card covers \"Read a charge\"", "code": "uncovered"}, false, true},
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
	// Not marked as one a redraft mends (or from a tafs cloud that doesn't mark it): the owner read the card,
	// and the refusal came at the tap.
	if len(tt.approvals) != 1 {
		t.Fatalf("the owner read %d approvals, want 1", len(tt.approvals))
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

// Eval 43690861487's Refunds: the owner read and approved a Card that the
// check then refused, and approved the redraft, two Card approvals for one
// app. tafs cloud now checks a connected app's Card as the phone reads the
// chat: a refusal a new draft mends (marked redraft) goes to Coach before the
// card is drawn, so the owner never reads, or approves, that card.
func TestARedraftRefusalReachesCoachBeforeTheOwnerReadsTheCard(t *testing.T) {
	refused := map[string]any{"outcome": "answered", "code": "vendor_name", "redraft": true, "answer": `failed: the Card covers "list_charges", but …`}
	tt, f := checkWorld(t, refused)
	tt.cfg.transcript = &strings.Builder{}
	c := component{Type: "approval", ID: "a1", Grant: "grt_1", Draft: "drf_1"}
	var approved string
	reread, answer, err := tt.approve(context.Background(), c, &approved)
	if err != nil || reread || answer != refused["answer"] {
		t.Fatalf("reread %v, answer %q, err %v", reread, answer, err)
	}
	if len(tt.approvals) != 0 || len(tt.draftIDs) != 0 || strings.Join(tt.draftRefusals, ",") != "vendor_name" {
		t.Fatalf("the owner read %d approvals, confirmed %v, refusals %v", len(tt.approvals), tt.draftIDs, tt.draftRefusals)
	}
	tr := tt.cfg.transcript.(*strings.Builder).String()
	if strings.Contains(tr, "the owner reads") || !strings.Contains(tr, "before the card was drawn") {
		t.Fatalf("transcript: %s", tr)
	}
	// The redraft passes: read once by the owner, checked once, confirmed.
	f.answer, f.calls = map[string]any{"outcome": "ok", "version": "v3"}, nil
	c.ID, c.Draft = "a2", "drf_2"
	if _, answer, err = tt.approve(context.Background(), c, &approved); err != nil || answer != "" {
		t.Fatalf("answer %q, err %v", answer, err)
	}
	if strings.Join(f.calls, "\n") != "kiff GET /v1/me/agent-drafts/drf_2\ncloud POST /v1/connect/grants/grt_1/check\nkiff POST /v1/me/agent-drafts/drf_2/confirm" ||
		len(tt.approvals) != 1 || strings.Join(tt.draftIDs, ",") != "drf_2" {
		t.Fatalf("calls %v, approvals %d, confirmed %v", f.calls, len(tt.approvals), tt.draftIDs)
	}
}

// Eval 13's Refunds: the owner approved the tools with the Card in one tap,
// and the Card was then refused. A card that signs the Card with tools added
// to an app signed in already, or with a key, is checked on the read too, as
// tafs cloud does: the refusal reaches Coach before the owner reads it, and
// nothing is approved on the phone.
func TestACardSignedWithItsToolsOrKeyIsCheckedBeforeTheOwnerReads(t *testing.T) {
	refused := map[string]any{"outcome": "answered", "code": "lifetime_sum", "redraft": true, "answer": `failed: the draft caps "refund_charge"'s amount …`}
	for _, grant := range []string{"grt_adding", "grt_key"} {
		tt, f := checkWorld(t, refused)
		c := component{Type: "approval", ID: "a1", Grant: grant, Draft: "drf_1"}
		var approved string
		reread, answer, err := tt.approve(context.Background(), c, &approved)
		if err != nil || reread || answer != refused["answer"] {
			t.Fatalf("%s: reread %v, answer %q, err %v", grant, reread, answer, err)
		}
		if len(tt.approvals) != 0 || approved != "" || len(tt.draftIDs) != 0 || strings.Join(tt.draftRefusals, ",") != "lifetime_sum" {
			t.Fatalf("%s: read %d approvals, approved %q, confirmed %v, refusals %v", grant, len(tt.approvals), approved, tt.draftIDs, tt.draftRefusals)
		}
		if strings.Join(f.calls, "\n") != "kiff GET /v1/me/agent-drafts/drf_1\ncloud POST /v1/connect/grants/"+grant+"/check" {
			t.Fatalf("%s: calls %v", grant, f.calls)
		}
	}
}

// Which cards the read checks is tafs cloud's word (checked_on_read), asked
// with what the owner's app can sign, never a copy of its rule here (the copy
// differed from tafs cloud's in two steps, so a refusal the owner would meet
// at the tap didn't show in an Eval).
func TestWhichCardsTheReadChecksIsTafsCloudsWord(t *testing.T) {
	refused := map[string]any{"outcome": "answered", "code": "vendor_name", "redraft": true, "answer": `failed: the Card covers "list_charges", but …`}
	tt, f := checkWorld(t, refused)
	c := component{Type: "approval", ID: "a1", Grant: "grt_1", Draft: "drf_1"}
	var approved string
	if _, answer, err := tt.approve(context.Background(), c, &approved); err != nil || answer != refused["answer"] {
		t.Fatalf("checked on the read: answer %q, err %v", answer, err)
	}
	// Asked as the app reads: with what it can sign.
	can, _ := f.asked["can"].([]any)
	if fmt.Sprint(can) != fmt.Sprint([]any{"tools_and_cards", "key_and_cards", "signs"}) {
		t.Fatalf("the check was asked with can %v", f.asked["can"])
	}
	// The same connected grant, where tafs cloud says the read doesn't check it:
	// the owner reads the card, whatever its status suggests.
	tt, f = checkWorld(t, refused)
	f.onRead["grt_1"] = false
	// Refused at the tap instead, after the owner read it, as tafs cloud would.
	if _, answer, err := tt.approve(context.Background(), c, &approved); err != nil || answer != refused["answer"] || len(tt.approvals) != 1 {
		t.Fatalf("not checked on the read: answer %q, err %v, the owner read %d approvals", answer, err, len(tt.approvals))
	}
	tr := tt.cfg.log.(*redacting).w.(*strings.Builder).String()
	if strings.Contains(tr, "before the card was drawn") {
		t.Fatalf("a card tafs cloud doesn't check on the read was refused before the owner read it:\n%s", tr)
	}
}

// Only a card that signs the Card is checked on the read, as tafs cloud
// does: any other card is read by the owner first, and checked after.
func TestOnlyAConnectedAppsCardIsCheckedBeforeTheOwnerReads(t *testing.T) {
	refused := map[string]any{"outcome": "answered", "code": "vendor_name", "redraft": true, "answer": `failed: the Card covers "list_charges", but …`}
	tt, _ := checkWorld(t, refused)
	c := component{Type: "approval", ID: "a1", Grant: "grt_listed", Draft: "drf_1"}
	var approved string
	if _, answer, err := tt.approve(context.Background(), c, &approved); err != nil || answer != refused["answer"] {
		t.Fatalf("answer %q, err %v", answer, err)
	}
	if len(tt.approvals) != 1 {
		t.Fatalf("the owner read %d approvals, want 1", len(tt.approvals))
	}
}

// Eval 13's Refunds: after a refused draft, Coach showed the result over its
// redraft with no approval of it. tafs cloud answers such a result back to
// Coach at once, in its own name; so does the harness, and counts it.
func TestAResultOverAnUnconfirmedDraftIsAnsweredBackByTafsCloud(t *testing.T) {
	c := component{Type: "result", ID: "done"}
	c.Taf = &struct {
		Agent        string `json:"agent"`
		Name         string `json:"name"`
		Job          string `json:"job"`
		Draft        string `json:"draft"`
		Improves     string `json:"improves"`
		Note         string `json:"note"`
		Notes        string `json:"notes"`
		Kickoff      string `json:"kickoff"`
		Instructions string `json:"instructions"`
		Disclose     []struct {
			Tool   string   `json:"tool"`
			Fields []string `json:"fields"`
		} `json:"disclose"`
	}{Name: "Refunds", Draft: "drf_2"}
	msg := unconfirmedAnswer(c)
	if msg != `[result done] {"failed":"The Card for Refunds was never approved, so Refunds isn't set up yet: approve its Card to finish"}` {
		t.Fatalf("the answer: %s", msg)
	}
	var sc show.Component
	raw, _ := json.Marshal(c)
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatal(err)
	}
	out, err := cardAnswer(sc, msg)
	if err != nil {
		t.Fatal(err)
	}
	var a show.Answered
	if err := json.Unmarshal(out, &a); err != nil {
		t.Fatal(err)
	}
	if a.AnsweredBy != "tafs cloud" || !strings.Contains(string(a.Answer), "was never approved") {
		t.Fatalf("answered %+v", a)
	}
	if got := unconfirmedLine(1); got != "setup results answered back, their draft not confirmed: 1" {
		t.Fatalf("the log says %q", got)
	}
}

// A result is answered after the cards beside it: an approval shown with it
// is confirmed first, so its result is never answered back for that.
func TestResultsAreAnsweredLast(t *testing.T) {
	got := resultsLast([]component{{Type: "result", ID: "r"}, {Type: "approval", ID: "a"}, {Type: "questions", ID: "q"}})
	var ids []string
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	if strings.Join(ids, ",") != "a,q,r" {
		t.Fatalf("order %v", ids)
	}
}

// Eval 14's Refunds: the transcript had the owner's "agree" to the rules and
// nothing of what was agreed. A notice and a confirm card's words are printed
// in the transcript, whole, once per card, and again when they change; never
// in the public log; other cards, and a card with no words, print nothing.
func TestTheOwnerReadsANoticeAndAConfirmInTheTranscript(t *testing.T) {
	tr := &strings.Builder{}
	pub := &strings.Builder{}
	tt := &test{out: &redacting{w: pub, ci: true}, cfg: config{log: pub, transcript: tr}}
	long := strings.Repeat("each refund up to €5, ", 20) + "and no more than 1,000 refunds a day"
	notice := component{Type: "notice", ID: "rules", Title: "Refunds' rules", Text: "  " + long + "\n"}
	tt.noteRead(notice)
	tt.noteRead(notice)
	tt.noteRead(component{Type: "confirm", ID: "rules-confirm-1", Text: "Agree to these rules?"})
	tt.noteRead(component{Type: "approval", ID: "a1", Text: "Approve to connect Stripe"})
	tt.noteRead(component{Type: "notice", ID: "empty"})
	notice.Text = "Up to €5 each, €50 a day"
	tt.noteRead(notice)
	got := strings.Split(strings.TrimSpace(tr.String()), "\n")
	want := []string{
		"the owner reads the notice rules: Refunds' rules: " + strings.TrimSpace(long),
		"the owner reads the confirm rules-confirm-1: Agree to these rules?",
		"the owner reads the notice rules: Refunds' rules: Up to €5 each, €50 a day",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("transcript:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Whole: the limit at the end of a long notice is there.
	if !strings.Contains(tr.String(), "1,000 refunds a day") {
		t.Fatal("the notice was cut")
	}
	if strings.Contains(pub.String(), "refund") {
		t.Fatalf("the public log has the notice: %s", pub.String())
	}
}
