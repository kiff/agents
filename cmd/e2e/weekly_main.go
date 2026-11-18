package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/kiff/agents/cmd/internal/testaccount"
	"github.com/kiff/agents/tool/kiff"
)

// weeklyFamilies are the weekly proof's families (PM 05:32, kiff-qa 05:27):
// one real Coach chat each, approved through tafs cloud's component route.
// Only key is set up today; a managed sign-in stays on the emulator pass
// until one completes without a consent page (integrations-prover 08:13).
var weeklyFamilies = map[string]weeklyFamily{
	"key": {
		Name:    "key",
		Ask:     "Connect Pipedrive with its API key so a taf can list my people, and add a follow-up activity only after I approve each one. Name the taf %s.",
		Fact:    "Use the Pipedrive account I connect with its key, and only the people list and adding an activity.",
		Job:     exactly("pipedrive_list_persons", "pipedrive_add_activity"),
		CapUSD:  3,
		MaxTurn: 8,
	},
	// own-key (kiff-qa 08:47): a vendor that isn't a catalog entry, by the
	// owner's own key as a raw request: planque's fake vendor desk, its
	// operations held to the desk's own description.
	"own-key": {
		Name: "own-key",
		Ask: "Our support team answers customers in our help desk. I want a taf that replies to customers' tickets and labels them, " +
			"in our workspace ws_demo only. The desk has no MCP server: its API is described at %[2]s/openapi.json, its operations start at %[2]s/v1, " +
			"and its key goes in the X-Desk-Key header. Name the taf %[1]s.",
		Fact:    "Up to 20 replies a day and 50 labels a day, nothing over that. It never touches another workspace, and never closes or deletes tickets.",
		Job:     within("searchTickets", "fetchTicket", "postReply", "applyLabel"),
		CapUSD:  3,
		MaxTurn: 8,
	},
}

// weeklyKeyEnv is the sandbox key each family's app takes, from kiff-test.
var weeklyKeyEnv = map[string]string{"key": "PROOF_PIPEDRIVE_KEY", "own-key": "E2E_DESK_KEY"}

// runWeeklyMode is -weekly <family>: a real Coach chat on tafs cloud, as the
// owner's phone, on the test account only (the daily proof's pin), its
// approval through the component route alone, and after the hand-over the
// taf's kept tools checked both ways. Whatever happens, what this run made is
// cleaned up, and earlier runs' proof-weekly-<family>- tafs and grants with it.
func runWeeklyMode(ctx context.Context, family string, stdout *redacting, env func(string) string) error {
	f, ok := weeklyFamilies[family]
	if !ok {
		return fmt.Errorf("-weekly %q: no such family set up", family)
	}
	cfg := config{log: stdout, phone: env("PROOF_PHONE_KEY"), session: env("PROOF_KIFF_SESSION"), setupKey: env("E2E_SETUP_KEY")}
	cfg.planque = firstOf(env("E2E_PLANQUE"), "https://planque.kiff.dev")
	cfg.cloud = firstOf(env("E2E_TAFS_CLOUD"), "https://cloud.tafs.kiff.dev")
	tenant, vendorKey := env("PROOF_TENANT"), env(weeklyKeyEnv[family])
	for k, v := range map[string]string{"PROOF_TENANT": tenant, "PROOF_PHONE_KEY": cfg.phone, "PROOF_KIFF_SESSION": cfg.session, weeklyKeyEnv[family]: vendorKey} {
		if v == "" {
			return fmt.Errorf("%s is required (from the kiff-test environment)", k)
		}
	}
	stdout.hide(vendorKey, "<the sandbox's key>")
	stdout.hide(cfg.session, "<the session>")
	hc := &http.Client{Timeout: 60 * time.Second}
	t := &test{cfg: cfg, out: stdout, hc: hc, run: time.Now().UTC().Format("0102-150405") + "-" + firstOf(env("GITHUB_RUN_ATTEMPT"), "1")}
	pk, err := testaccount.ParsePhone(cfg.phone)
	if err != nil {
		return err
	}
	t.pk = pk
	me, err := testaccount.KIFFSession(kiff.APIURL, cfg.session, "", pk, hc).Do(ctx, http.MethodGet, "/v1/me", nil)
	if err != nil || me.Err() != nil {
		return fmt.Errorf("KIFF didn't take the proof phone's session: %v %v", err, me.Err())
	}
	t.tenant, _ = me.Body["tenant_id"].(string)
	stdout.hide(t.tenant, "<the account>")
	fp, err := keyFingerprint(&pk.PublicKey)
	if err != nil {
		return err
	}
	if err := proofPin(t.tenant, tenant, fp); err != nil {
		return fmt.Errorf("the weekly proof did nothing: %w", err)
	}
	t.kiff = testaccount.KIFFSession(kiff.APIURL, cfg.session, t.tenant, pk, hc)
	t.phone = testaccount.NewPhone(pk, cfg.planque, t.tenant, hc)
	t.phone.VendorKey = vendorKey
	if t.phone.Device, err = testaccount.ThisDevice(ctx, t.kiff); err != nil {
		return err
	}
	chat := &liveWeeklyChat{t: t}
	if chat.coach, err = coachOf(ctx, chat.cloud()); err != nil {
		// Never start a Coach from the proof (agentgabo 08:14).
		return err
	}
	name := "proof-weekly-" + f.Name + "-" + t.run
	if f.Name == "own-key" {
		// Where the fake vendor's desk is: E2E_VENDOR, as for the Eval's -desk.
		vendor := env("E2E_VENDOR")
		if vendor == "" {
			return errors.New("E2E_VENDOR is required for the own-key family (from the kiff-test environment)")
		}
		f.Ask = fmt.Sprintf(f.Ask, name, deskOf(vendor))
	} else {
		f.Ask = fmt.Sprintf(f.Ask, name)
	}
	defer func() {
		n, err := sweepWeekly(context.WithoutCancel(ctx), t, f.Name)
		fmt.Fprintf(stdout, "  cleanup: %d left by this run or earlier ones taken away (%v)\n", n, err)
	}()

	fmt.Fprintf(stdout, "== weekly proof, %s family\n", f.Name)
	r, err := driveCoach(ctx, chat, &liveWeeklyCloud{t: t, coach: chat.coach}, liveWeeklyPhone{t: t}, f, t.tenant, pollWait(5*time.Second))
	fmt.Fprintf(stdout, "  turns %d, spent US$%.2f of US$%.2f, approvals %d, taps %d\n", r.Turns, float64(r.Spent)/1e6, f.CapUSD, len(r.Approvals), len(r.Taps))
	for _, tap := range r.Taps {
		fmt.Fprintf(stdout, "  tapped %q (agrees to %q)\n", tap.Words, tap.Agrees)
	}
	if err != nil {
		return err
	}
	var conn []string
	for _, g := range r.Grants {
		pg, err := t.phone.Grant(ctx, g)
		if err != nil {
			return err
		}
		for _, tl := range pg.Request.Tools {
			conn = append(conn, tl.Name)
		}
	}
	after, err := liveTafTools{c: chat}.TafTools(ctx, r.Taf)
	if err != nil {
		return err
	}
	// The taf is new, made by the hand-over: it had no tools before. What the
	// signed card showed is the connection's own tools (sameTools held).
	if err := toolsKept(nil, after, conn, conn); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "  the taf kept exactly the approved connection's tools: PASS")
	return nil
}

// sweepWeekly takes away what weekly proofs of a family made: the grants the
// setup agent asked for proof-weekly-<family>- agents, and those agents, and
// nothing else (the daily proof's narrow sweep).
func sweepWeekly(ctx context.Context, t *test, family string) (int, error) {
	prefix := "weekly-" + family
	p := &liveProof{t: t}
	if t.cfg.setupKey == "" {
		return 0, errors.New("no setup key to list the asked grants")
	}
	return p.Sweep(ctx, proofApp{Name: prefix})
}

// weeklyFamilyNames are the families -weekly takes, in order.
func weeklyFamilyNames() []string {
	var out []string
	for n := range weeklyFamilies {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}
