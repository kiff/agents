package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeProof is a proof's steps on nothing: each answer is the test's.
type fakeProof struct {
	connectErr error
	draftOK    bool
	draftCode  string
	approveErr error
	read       proofCall
	held       proofCall
	refused    string
	after      proofCall
	cleanupErr error

	calls    []string // the tools called, by name
	heldArgs map[string]any
	cleaned  bool
	cards    []map[string]any
}

func (f *fakeProof) Sweep(context.Context, proofApp) (int, error) { return 0, nil }

func (f *fakeProof) Connect(_ context.Context, app proofApp, name string, tools []string) (proofGrant, error) {
	if f.connectErr != nil {
		return proofGrant{}, f.connectErr
	}
	g := proofGrant{ID: "grt_fake", Agent: name + "-a1b2c", Names: map[string]string{}}
	for _, t := range tools {
		g.Names[t] = t + "_x9k2"
	}
	return g, nil
}

func (f *fakeProof) Draft(_ context.Context, _ proofGrant, _ string, cards []map[string]any) (string, bool, string, error) {
	f.cards = cards
	return "drf_fake", f.draftOK, f.draftCode, nil
}

func (f *fakeProof) Approve(context.Context, proofGrant, string) error { return f.approveErr }

func (f *fakeProof) Call(_ context.Context, g proofGrant, name string, args map[string]any) (proofCall, error) {
	f.calls = append(f.calls, name)
	switch len(f.calls) {
	case 1:
		return f.read, nil
	case 2:
		f.heldArgs = args
		return f.held, nil
	default:
		return f.after, nil
	}
}

func (f *fakeProof) Refuse(context.Context, string) (string, error) { return f.refused, nil }

func (f *fakeProof) Cleanup(context.Context, proofGrant) error {
	f.cleaned = true
	return f.cleanupErr
}

func listed(n int, names ...string) proofCall {
	c := proofCall{State: "forwarded", Count: n}
	for i := 0; i < n; i++ {
		name := ""
		if i < len(names) {
			name = names[i]
		}
		c.Items = append(c.Items, proofItem{ID: "id" + string(rune('a'+i)), Name: name})
	}
	return c
}

var testNow = time.Date(2026, 11, 18, 5, 0, 0, 0, time.UTC)

func clock() func() time.Time {
	t := testNow
	return func() time.Time { t = t.Add(100 * time.Millisecond); return t }
}

func checksOf(r proofResult) map[string]proofCheck {
	m := map[string]proofCheck{}
	for _, c := range r.Checks {
		m[c.Name] = c
	}
	return m
}

func TestAReadOnlyProofPassesWhenTheCallOutsideTheCardIsRefused(t *testing.T) {
	f := &fakeProof{draftOK: true, read: listed(1, "Contact us"), held: proofCall{State: "refused"}}
	r := runProof(context.Background(), proofApps["typeform"], "1118-0500", f, clock())
	if r.Outcome != "pass" {
		t.Fatalf("outcome %s, checks %+v", r.Outcome, r.Checks)
	}
	var names []string
	for _, c := range r.Checks {
		names = append(names, c.Name)
	}
	if want := []string{"connects", "words", "approval_applied", "reads", "card_limit_held"}; !slices.Equal(names, want) {
		t.Fatalf("checks %v; want %v", names, want)
	}
	if got := checksOf(r)["reads"].Why; got != "listed 1" {
		t.Fatalf("reads says %q; want the count", got)
	}
	if !f.cleaned {
		t.Fatal("the grant and agent weren't cleaned up")
	}
	// The held call is on the grant (so KIFF, not planque, refuses it) and on no Card.
	if !slices.Contains(f.calls, "typeform_list_responses_x9k2") {
		t.Fatalf("calls %v: the held call isn't by the grant's name", f.calls)
	}
	for _, c := range f.cards {
		b, _ := json.Marshal(c)
		if strings.Contains(string(b), "typeform_list_responses") {
			t.Fatalf("a Card covers the call that must be refused: %s", b)
		}
	}
}

func TestAReadOnlyProofFailsTheCardLimitWhenTheCallGoesThrough(t *testing.T) {
	f := &fakeProof{draftOK: true, read: listed(1, "Contact us"), held: proofCall{State: "forwarded"}}
	r := runProof(context.Background(), proofApps["typeform"], "1118-0500", f, clock())
	if c := checksOf(r)["card_limit_held"]; r.Outcome != "fail" || c.OK {
		t.Fatalf("outcome %s, card_limit_held %+v; want a failed limit", r.Outcome, c)
	}
	s := summaryOf(proofApps["typeform"], r, "gh-1-1-typeform", "tenant", "manual", strings.Repeat("a", 40), testNow, testNow)
	if s.LimitsHeld {
		t.Fatal("limits_held is true with card_limit_held failed")
	}
}

func TestAHeldWriteIsRefusedAndEndsRefusedWithTheSandboxUnchanged(t *testing.T) {
	f := &fakeProof{draftOK: true, read: listed(3), held: proofCall{State: "held", Hold: "exc_1"}, refused: "refused", after: listed(3)}
	r := runProof(context.Background(), proofApps["pipedrive"], "1118-0500", f, clock())
	if r.Outcome != "pass" {
		t.Fatalf("outcome %s, checks %+v", r.Outcome, r.Checks)
	}
	for _, n := range []string{"hold_raised", "writes", "card_limit_held", "vendor_received"} {
		if !checksOf(r)[n].OK {
			t.Fatalf("%s failed: %+v", n, r.Checks)
		}
	}
	// The write's Card asks the owner about every call.
	var writes map[string]any
	for _, c := range f.cards {
		if strings.HasSuffix(c["id"].(string), "-writes") {
			writes = c
		}
	}
	if writes == nil || writes["on_exceed"] != "owner" {
		t.Fatalf("no write Card that holds for the owner: %v", f.cards)
	}
	agg := writes["aggregates"].([]map[string]any)[0]
	if agg["quantity"] != "count" || agg["limit"] != 0 {
		t.Fatalf("the write Card's total is %v; want a count of 0", agg)
	}
}

func TestAHeldWriteThatDoesNotEndRefusedFails(t *testing.T) {
	f := &fakeProof{draftOK: true, read: listed(3), held: proofCall{State: "held", Hold: "exc_1"}, refused: "held", after: listed(3)}
	r := runProof(context.Background(), proofApps["pipedrive"], "1118-0500", f, clock())
	if c := checksOf(r)["writes"]; c.OK || !strings.Contains(c.Why, "not refused") {
		t.Fatalf("writes %+v; want it failed: the hold didn't end refused", c)
	}
}

func TestAWriteThatGoesThroughFailsTheLimitAndTheHold(t *testing.T) {
	f := &fakeProof{draftOK: true, read: listed(3), held: proofCall{State: "forwarded"}, after: listed(4)}
	r := runProof(context.Background(), proofApps["pipedrive"], "1118-0500", f, clock())
	c := checksOf(r)
	if r.Outcome != "fail" || c["card_limit_held"].OK || c["hold_raised"].OK {
		t.Fatalf("outcome %s, checks %+v", r.Outcome, r.Checks)
	}
	if c["writes"].Why != "not reached" || c["vendor_received"].Why != "not reached" {
		t.Fatalf("checks after a write that went through ran: %+v", r.Checks)
	}
}

func TestTheSandboxChangedAfterARefusedWriteFails(t *testing.T) {
	f := &fakeProof{draftOK: true, read: listed(3), held: proofCall{State: "held", Hold: "exc_1"}, refused: "refused", after: listed(4)}
	r := runProof(context.Background(), proofApps["pipedrive"], "1118-0500", f, clock())
	if c := checksOf(r)["vendor_received"]; c.OK {
		t.Fatalf("vendor_received %+v with one more person after the refusal", c)
	}
}

func TestAnEmptyListFailsOnlyWhereTheSandboxIsSeeded(t *testing.T) {
	seeded := runProof(context.Background(), proofApps["typeform"], "r", &fakeProof{draftOK: true, read: listed(0), held: proofCall{State: "refused"}}, clock())
	if c := checksOf(seeded)["reads"]; c.OK || !strings.Contains(c.Why, "listed 0") {
		t.Fatalf("a seeded sandbox listing nothing: reads %+v", c)
	}
	unseeded := runProof(context.Background(), proofApps["pipedrive"], "r",
		&fakeProof{draftOK: true, read: listed(0), held: proofCall{State: "held", Hold: "exc"}, refused: "refused", after: listed(0)}, clock())
	if c := checksOf(unseeded)["reads"]; !c.OK || c.Why != "listed 0" {
		t.Fatalf("an unseeded sandbox listing nothing: reads %+v", c)
	}
}

func TestMoneybirdActsOnlyOnTheTestBooks(t *testing.T) {
	f := &fakeProof{draftOK: true, read: listed(2, "Kiff Agents OÜ", "KIFF Test Books"), held: proofCall{State: "refused"}}
	r := runProof(context.Background(), proofApps["moneybird"], "r", f, clock())
	if r.Outcome != "pass" {
		t.Fatalf("outcome %s: %+v", r.Outcome, r.Checks)
	}
	if got := f.heldArgs["administration_id"]; got != "idb" {
		t.Fatalf("the held call names administration %v; want the test books' (idb)", got)
	}
	f = &fakeProof{draftOK: true, read: listed(1, "Kiff Agents OÜ"), held: proofCall{State: "refused"}}
	r = runProof(context.Background(), proofApps["moneybird"], "r", f, clock())
	if c := checksOf(r)["reads"]; c.OK || len(f.calls) != 1 {
		t.Fatalf("without the test books: reads %+v, calls %v; want it failed and nothing called after", c, f.calls)
	}
}

func TestAProofThatDoesNotConnectSaysWhyAndReachesNothingElse(t *testing.T) {
	f := &fakeProof{connectErr: errors.New("planque refused the request: no such catalog entry")}
	r := runProof(context.Background(), proofApps["typeform"], "r", f, clock())
	c := checksOf(r)
	if c["connects"].OK || !strings.Contains(c["connects"].Why, "no such catalog entry") {
		t.Fatalf("connects %+v", c["connects"])
	}
	if c["reads"].Why != "not reached" || f.cleaned {
		t.Fatalf("checks %+v, cleaned %v: nothing after a failed connect", r.Checks, f.cleaned)
	}
}

func TestARefusedDraftFailsWordsWithTheCheckCodeAndCleansUp(t *testing.T) {
	f := &fakeProof{draftOK: false, draftCode: "rules_missing"}
	r := runProof(context.Background(), proofApps["pipedrive"], "r", f, clock())
	if c := checksOf(r)["words"]; c.OK || !strings.Contains(c.Why, "rules_missing") {
		t.Fatalf("words %+v", c)
	}
	if !f.cleaned {
		t.Fatal("a refused draft's grant was left connected")
	}
}

func TestACleanupThatFailsIsSaid(t *testing.T) {
	f := &fakeProof{draftOK: true, read: listed(1, "x"), held: proofCall{State: "refused"}, cleanupErr: errors.New("archive: 500")}
	r := runProof(context.Background(), proofApps["typeform"], "r", f, clock())
	if !strings.Contains(r.Error, "cleanup") {
		t.Fatalf("the failed cleanup isn't said: %q", r.Error)
	}
}

func TestAnExpiredTrialSaysAccountExpired(t *testing.T) {
	app := proofApps["pipedrive"]
	late := func() time.Time { return app.Expires.Add(time.Hour) }
	r := runProof(context.Background(), app, "r", &fakeProof{connectErr: errors.New("401")}, late)
	if !r.Expired || checksOf(r)["connects"].Why != "account expired" {
		t.Fatalf("expired %v, checks %+v", r.Expired, r.Checks)
	}
	var b bytes.Buffer
	printProof(&b, app, r)
	if !strings.Contains(b.String(), "isn't the app failing") {
		t.Fatalf("the log doesn't say the account expired:\n%s", b.String())
	}
	// Before the end, the same failure is the app's.
	r = runProof(context.Background(), app, "r", &fakeProof{connectErr: errors.New("401")}, clock())
	if r.Expired {
		t.Fatal("expired before the trial's end")
	}
}

func TestTheSweepTakesOnlyThisAppsProofAgents(t *testing.T) {
	for _, c := range []struct {
		agent string
		swept bool
	}{
		{"proof-pipedrive-1118-0500-a1b2c", true},
		{"proof-typeform-1118-0500-a1b2c", false}, // another app's proof
		{"pipedrive-refunds-a1b2c", false},        // a person's taf
		{"e2e-setup", false},                      // Setup's agent
		{"eval-proof-pipedrive-x", false},
		{"", false},
	} {
		if got := proofSwept("pipedrive", c.agent, "e2e-setup"); got != c.swept {
			t.Errorf("proofSwept(%q) = %v; want %v", c.agent, got, c.swept)
		}
	}
	if proofSwept("pipedrive", "proof-pipedrive-x", "proof-pipedrive-x") {
		t.Error("Setup's agent swept")
	}
}

func TestTheGrantsNamesAreTheCatalogsWithTheSuffix(t *testing.T) {
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"pipedrive_list_persons", true},
		{"pipedrive_list_persons_x9k2", true},
		{"pipedrive_list_persons_x9k", false},
		{"pipedrive_list_persons_all", false},
		{"pipedrive_list_personsx9k2", false},
	} {
		if got := tafNameOf(c.name, "pipedrive_list_persons"); got != c.ok {
			t.Errorf("tafNameOf(%q) = %v; want %v", c.name, got, c.ok)
		}
	}
}

func TestAListIsReadFromItsUsualPlaces(t *testing.T) {
	for raw, n := range map[string]int{
		`[{"id":1,"name":"a"},{"id":2,"name":"b"}]`:                   2,
		`{"items":[{"id":"x","title":"Contact us"}],"total_items":1}`: 1,
		`{"success":true,"data":[]}`:                                  0,
		`{"results":[{"id":1}]}`:                                      1,
		`{"id":1}`:                                                    -1,
		`not json`:                                                    -1,
	} {
		if _, got := listOf([]byte(raw)); got != n {
			t.Errorf("listOf(%s) = %d; want %d", raw, got, n)
		}
	}
	items, _ := listOf([]byte(`{"items":[{"id":"abc","title":"Contact us"}]}`))
	if items[0].ID != "abc" || items[0].Name != "Contact us" {
		t.Fatalf("items %+v", items)
	}
}

func TestASummaryWithAKeyOrAnAddressIsNeverPosted(t *testing.T) {
	posted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posted = true }))
	defer srv.Close()
	for _, why := range []string{"sent to owner@example.com", "key sk_test_51Habcdefghijklmnopqrst", "grant grt_BRQVabc1 refused"} {
		s := proofSummary{Checks: []proofCheck{{Name: "connects", Why: why}}, Rules: json.RawMessage(`{}`)}
		if err := postProof(context.Background(), srv.Client(), srv.URL, "tok", s); err == nil || posted {
			t.Fatalf("a summary saying %q was posted", why)
		}
	}
	// What a why becomes before it's kept: nothing of the kind left.
	got := scrubWhy("KIFF refused: owner@example.com, grt_BRQVabc1, at https://x.test/a, key AbCdEf1234567890xyz")
	if summaryLeaks(proofSummary{Checks: []proofCheck{{Why: got}}}) != nil {
		t.Fatalf("scrubbed why still leaks: %q", got)
	}
}

func TestAProofPostThatFailsFailsTheJob(t *testing.T) {
	var auth, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		if r.URL.Path != "/v1/ops/proofs" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if strings.Contains(body, `"outcome":"fail"`) {
			http.Error(w, `{"error":"invalid run: check \"x\""}`, http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	r := runProof(context.Background(), proofApps["typeform"], "r", &fakeProof{draftOK: true, read: listed(1, "Contact us"), held: proofCall{State: "refused"}}, clock())
	s := summaryOf(proofApps["typeform"], r, "gh-123-1-typeform", "tenant", "manual", strings.Repeat("a", 40), testNow, testNow.Add(time.Minute))
	if err := postProof(context.Background(), srv.Client(), srv.URL, "tok", s); err != nil {
		t.Fatalf("a good post: %v", err)
	}
	if auth != "Bearer tok" || !strings.Contains(body, `"events_key":"proofs/gh-123-1-typeform"`) || !strings.Contains(body, `"entry":"typeform-key@1"`) {
		t.Fatalf("posted %q with %q", body, auth)
	}
	s.Outcome = "fail"
	if err := postProof(context.Background(), srv.Client(), srv.URL, "tok", s); err == nil {
		t.Fatal("a refused post didn't fail")
	}
	srv.Close()
	if err := postProof(context.Background(), srv.Client(), srv.URL, "tok", s); err == nil {
		t.Fatal("an unreachable KIFF didn't fail")
	}
}

func TestASummaryHoldsItsOutcomeToItsChecks(t *testing.T) {
	for _, name := range proofAppNames() {
		app := proofApps[name]
		r := runProof(context.Background(), app, "r", &fakeProof{connectErr: errors.New("x")}, clock())
		s := summaryOf(app, r, "gh-1-1-"+name, "tenant", "schedule", strings.Repeat("a", 40), testNow, testNow)
		names := map[string]bool{}
		allOK := true
		for _, c := range s.Checks {
			names[c.Name] = true
			allOK = allOK && c.OK
		}
		if !names["connects"] || !names["card_limit_held"] {
			t.Errorf("%s: the required checks aren't all there: %v", name, names)
		}
		if (s.Outcome == "pass") != allOK || s.P50Ms > s.P95Ms || !proofRunID.MatchString(s.RunID) {
			t.Errorf("%s: outcome %s, p50 %d, p95 %d, run %q", name, s.Outcome, s.P50Ms, s.P95Ms, s.RunID)
		}
	}
}

func TestTheDryRunNeedsNoKey(t *testing.T) {
	var b bytes.Buffer
	env := func(string) string { return "" }
	for _, name := range proofAppNames() {
		b.Reset()
		if err := run(context.Background(), []string{"-proof", name, "-proof-dry"}, &redacting{w: &b}, env); err != nil {
			t.Fatalf("-proof %s -proof-dry: %v\n%s", name, err, b.String())
		}
		if !strings.Contains(b.String(), "nothing posted") {
			t.Fatalf("%s:\n%s", name, b.String())
		}
	}
	if err := run(context.Background(), []string{"-proof", "pipedrive"}, &redacting{w: &b}, env); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("a real proof with no keys: %v", err)
	}
	if err := run(context.Background(), []string{"-proof", "stripe", "-proof-dry"}, &redacting{w: &b}, env); err == nil {
		t.Fatal("a proof of an app with no sandbox ran")
	}
}

func TestPercentilesAreNearestRank(t *testing.T) {
	ds := []time.Duration{400 * time.Millisecond, 100 * time.Millisecond, 300 * time.Millisecond, 200 * time.Millisecond}
	if p50, p95 := percentile(ds, 50), percentile(ds, 95); p50 != 200 || p95 != 400 {
		t.Fatalf("p50 %d, p95 %d; want 200, 400", p50, p95)
	}
	if percentile(nil, 50) != 0 {
		t.Fatal("no timings isn't 0")
	}
}
