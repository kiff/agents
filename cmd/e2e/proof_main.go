package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/kiff/agents/cmd/internal/testaccount"
	"github.com/kiff/agents/tool/kiff"
)

// proofAudience is the audience KIFF takes the proofs' OIDC tokens for.
const proofAudience = "kiff-proofs"

var gitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// runProofMode is -proof <app>: the app's proof on the test account, posted
// to KIFF. With dry, nothing reaches the account, the vendor or KIFF: it
// prints what the proof asks for, the Cards and the checks, and holds an
// unrun summary to what KIFF takes, with no key needed.
func runProofMode(ctx context.Context, name string, dry bool, stdout *redacting, env func(string) string) error {
	app, ok := proofApps[name]
	if !ok {
		return fmt.Errorf("-proof %q: one of %s", name, strings.Join(proofAppNames(), ", "))
	}
	runID := proofRunIDOf(env, app.Name)
	trigger := "manual"
	if env("GITHUB_EVENT_NAME") == "schedule" {
		trigger = "schedule"
	}
	if dry {
		return dryProof(stdout, app, runID, trigger)
	}
	cfg := config{log: stdout, phone: env("PROOF_PHONE_KEY"), session: env("PROOF_KIFF_SESSION"), setupKey: env("E2E_SETUP_KEY")}
	cfg.planque = firstOf(env("E2E_PLANQUE"), "https://planque.kiff.dev")
	cfg.cloud = firstOf(env("E2E_TAFS_CLOUD"), "https://cloud.tafs.kiff.dev")
	vendorKey := env(app.KeyEnv)
	tenant := env("PROOF_TENANT")
	for k, v := range map[string]string{"PROOF_TENANT": tenant, "PROOF_PHONE_KEY": cfg.phone, "PROOF_KIFF_SESSION": cfg.session, "E2E_SETUP_KEY": cfg.setupKey, app.KeyEnv: vendorKey} {
		if v == "" {
			return fmt.Errorf("%s is required (from the kiff-test environment); -proof-dry needs none", k)
		}
	}
	version := env("GITHUB_SHA")
	if !gitSHA.MatchString(version) {
		return errors.New("-proof posts only from a GitHub run (GITHUB_SHA): by hand, use -proof-dry")
	}
	hc := &http.Client{Timeout: 60 * time.Second}
	token, err := oidcToken(ctx, hc, env, proofAudience)
	if err != nil {
		return err
	}
	stdout.hide(token, "<the run's token>")
	stdout.hide(vendorKey, "<the sandbox's key>")
	stdout.hide(cfg.session, "<the session>")
	stdout.hide(cfg.setupKey, "<the setup key>")

	t := &test{cfg: cfg, out: stdout, hc: hc, run: time.Now().UTC().Format("0102-1504")}
	pk, err := testaccount.ParsePhone(cfg.phone)
	if err != nil {
		return err
	}
	t.pk = pk
	probe := testaccount.KIFFSession(kiff.APIURL, cfg.session, "", pk, hc)
	me, err := probe.Do(ctx, http.MethodGet, "/v1/me", nil)
	if err != nil || me.Err() != nil {
		return fmt.Errorf("KIFF didn't take the proof phone's session: %v %v", err, me.Err())
	}
	t.tenant, _ = me.Body["tenant_id"].(string)
	stdout.hide(t.tenant, "<the account>")
	// Before anything acts on the account, the phone's enrolment included.
	fp, err := keyFingerprint(&pk.PublicKey)
	if err != nil {
		return err
	}
	if err := proofPin(t.tenant, tenant, fp); err != nil {
		return fmt.Errorf("the proof did nothing: %w", err)
	}
	t.kiff = testaccount.KIFFSession(kiff.APIURL, cfg.session, t.tenant, pk, hc)
	t.phone = testaccount.NewPhone(pk, cfg.planque, t.tenant, hc)
	t.phone.VendorKey = vendorKey
	if t.phone.Device, err = testaccount.ThisDevice(ctx, t.kiff); err != nil {
		return fmt.Errorf("the proof phone's device in KIFF: %w", err)
	}
	if err := t.enrol(ctx); err != nil {
		return err
	}
	_, setup, err := testaccount.PhoneAccess(ctx, t.kiff)
	if err != nil {
		return err
	}

	started := time.Now()
	r := runProof(ctx, app, t.run, newLiveProof(t, setup, tenant), time.Now)
	ended := time.Now()
	printProof(stdout, app, r)
	if r.Unpinned {
		return errors.New("the proof did nothing: " + scrubWhy(r.Error))
	}
	s := summaryOf(app, r, runID, t.tenant, trigger, version, started, ended)
	if err := postProof(ctx, hc, kiff.APIURL, token, s); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "posted to KIFF")
	switch {
	case r.Outcome == "error":
		return errors.New("the proof runner broke: " + scrubWhy(r.Error))
	case r.Error != "":
		// Posted, but something was left behind: the next run's sweep takes it.
		return errors.New(scrubWhy(r.Error))
	case r.Outcome != "pass":
		return fmt.Errorf("the proof of %s failed", app.Name)
	}
	return nil
}

// proofRunIDOf is the run's id: the GitHub run, its attempt and the app, so
// a re-run is a run of its own; "local-…" by hand.
func proofRunIDOf(env func(string) string, app string) string {
	if id := env("GITHUB_RUN_ID"); id != "" {
		return "gh-" + id + "-" + firstOf(env("GITHUB_RUN_ATTEMPT"), "1") + "-" + app
	}
	return "local-" + time.Now().UTC().Format("20060102-150405") + "-" + app
}

// dryProof prints app's proof as it would run, and holds an unrun summary
// to what KIFF takes.
func dryProof(w *redacting, app proofApp, runID, trigger string) error {
	fmt.Fprintf(w, "== proof of %s (%s), dry: nothing is asked, signed or posted\n", app.Name, app.Entry)
	fmt.Fprintf(w, "  asks for: %s\n", strings.Join(proofTools(app), ", "))
	g := proofGrant{Agent: proofAgentPrefix(app.Name) + "<run>", Names: map[string]string{}}
	for _, c := range proofTools(app) {
		g.Names[c] = c + "_<grant>"
	}
	cards, err := proofCards(app, g)
	if err != nil {
		return err
	}
	for _, c := range cards {
		fmt.Fprintf(w, "  Card %v: %v, %v\n", c["id"], c["grants"], c["aggregates"])
	}
	fmt.Fprintf(w, "  checks: %s\n", strings.Join(proofChecks(app), ", "))
	if !proofRunID.MatchString(runID) {
		return fmt.Errorf("the run id %q isn't one KIFF takes", runID)
	}
	var unrun proofResult
	for _, n := range proofChecks(app) {
		unrun.Checks = append(unrun.Checks, proofCheck{Name: n, Why: "not run (dry)"})
	}
	unrun.Outcome = "fail"
	now := time.Now()
	s := summaryOf(app, unrun, runID, "<the account>", trigger, strings.Repeat("0", 40), now, now)
	if leaks := summaryLeaks(s); len(leaks) > 0 {
		return fmt.Errorf("the summary would carry text shaped like a key, an address or an id: %v", leaks)
	}
	fmt.Fprintln(w, "  the summary's shape holds; nothing posted")
	return nil
}
