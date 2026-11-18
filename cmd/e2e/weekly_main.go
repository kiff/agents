package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/kiff/agents/cmd/internal/testaccount"
	"github.com/kiff/agents/tool/kiff"
)

// weeklyFamilyDeadline is how long one family may run, so both fit inside the
// job's 30 minutes with their cleanup.
const weeklyFamilyDeadline = 12 * time.Minute

// weeklyFamilies are the weekly proof's families:
// one real Coach chat each, approved through tafs cloud's component route.
// Only key is set up today; a managed sign-in stays on the emulator pass
// until one completes without a consent page.
var weeklyFamilies = map[string]weeklyFamily{
	"key": {
		Name:    "key",
		Ask:     "Connect Pipedrive with its API key so a taf can list my people, and add a follow-up activity only after I approve each one. Name the taf %s.",
		Fact:    "Use the Pipedrive account I connect with its key, and only the people list and adding an activity.",
		Job:     exactly("pipedrive_list_persons", "pipedrive_add_activity"),
		CapUSD:  3,
		MaxTurn: 8,
	},
	// own-key: a vendor that isn't a catalog entry, by the
	// owner's own key as a raw request: planque's fake vendor desk, its
	// operations held to the desk's own description.
	"own-key": {
		Name: "own-key",
		Ask: "Our support team answers customers in our help desk. I want a taf that reads our open tickets each morning and tells me which ones wait longest, " +
			"in our workspace ws_demo only. It only reads: it searches the tickets and fetches one to read it, and it never replies, labels or changes anything. " +
			"The desk has no MCP server: its API is described at %[2]s/openapi.json, its operations start at %[2]s/v1, " +
			"and its key goes in the X-Desk-Key header. Name the taf %[1]s.",
		Fact: "Only our workspace ws_demo, and it only reads tickets: searching them and fetching one. Up to 50 reads a day.",
		// The reading job's own operations, nothing more.
		Job:     operations(deskOperations, "searchTickets", "fetchTicket"),
		CapUSD:  3,
		MaxTurn: 8,
	},
}

// weeklyKeyEnv is the sandbox key each family's app takes, from kiff-test.
var weeklyKeyEnv = map[string]string{"key": "PROOF_PIPEDRIVE_KEY", "own-key": "E2E_DESK_KEY"}

// weeklyOwnEnv are the secrets a family alone needs. When one isn't set, the
// family is skipped, not proved, and the other family still runs;
// a second skip in a row fails the job. The account's own
// secrets, which both families share, are never a skip: they fail the job.
var weeklyOwnEnv = map[string][]string{"key": {"PROOF_PIPEDRIVE_KEY"}, "own-key": {"E2E_DESK_KEY", "E2E_VENDOR"}}

// weeklySkip skips family when a secret of its own isn't set: a warning, a
// "skipped, not proved" summary (never "passed") and skipped=true in the
// step's outputs, which the workflow keeps as a step of its own for the next
// run to read. When the last weekly run skipped the family too
// (WEEKLY_LAST_SKIPPED, the families it skipped), the skip fails the job.
func weeklySkip(f weeklyFamily, w io.Writer, env func(string) string) (bool, error) {
	var missing []string
	for _, k := range weeklyOwnEnv[f.Name] {
		if env(k) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return false, nil
	}
	why := strings.Join(missing, ", ") + " not set in kiff-test"
	if err := appendFile(env("GITHUB_OUTPUT"), "skipped=true\n"); err != nil {
		return true, fmt.Errorf("the %s family is skipped (%s), and its skip couldn't be kept for the next run: %w", f.Name, why, err)
	}
	if slices.Contains(strings.Fields(env("WEEKLY_LAST_SKIPPED")), f.Name) {
		writeSummary(env("GITHUB_STEP_SUMMARY"), fmt.Sprintf("### Weekly proof, %s family: FAIL: skipped twice in a row, not proved\n\n- %s.\n", f.Name, why))
		return true, fmt.Errorf("the %s family was skipped twice in a row, so it proves nothing: %s", f.Name, why)
	}
	fmt.Fprintf(w, "::warning title=Weekly proof, %s family::skipped, not proved: %s. A second skip in a row fails the job.\n", f.Name, why)
	writeSummary(env("GITHUB_STEP_SUMMARY"), fmt.Sprintf("### Weekly proof, %s family: skipped, not proved\n\n- %s.\n- A second skip in a row fails the job.\n", f.Name, why))
	return true, nil
}

// runWeeklyMode is -weekly <family>: a real Coach chat on tafs cloud, as the
// owner's phone, on the test account only (the daily proof's pin), its
// approval through the component route alone, and after the hand-over the
// taf's kept tools checked both ways. Whatever happens, what this run made is
// cleaned up, and earlier runs' proof-weekly-<family>- tafs and grants with it.
func runWeeklyMode(ctx context.Context, family string, stdout *redacting, env func(string) string) (retErr error) {
	f, ok := weeklyFamilies[family]
	if !ok {
		return fmt.Errorf("-weekly %q: no such family set up", family)
	}
	if skipped, err := weeklySkip(f, stdout, env); skipped {
		return err
	}
	// A family's own deadline, inside the job's: a Coach that stays at work
	// ends this family, not the job, so its cleanup and summary still run and
	// the next family still starts. The cleanup runs past it.
	ctx, cancel := context.WithTimeout(ctx, weeklyFamilyDeadline)
	defer cancel()
	t, chat, err := weeklyAccount(ctx, stdout, env, weeklyKeyEnv[family])
	if err != nil {
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
	// The tafs on the account before the run: a taf Coach hands over that was
	// already there (Coach installed into it) isn't this run's to archive.
	before, _, err := testaccount.PhoneAccess(ctx, t.kiff)
	if err != nil {
		return err
	}
	var r weeklyResult
	defer func() {
		// What this run recorded first, whatever Coach named it: its grants
		// disconnected, its taf archived. Then the name sweep, for what an
		// earlier run left.
		cleanCtx := context.WithoutCancel(ctx)
		for _, g := range r.Grants {
			stdout.hide(g, "<a grant>")
		}
		mine := weeklyCleanup(r, before, stdout,
			func(g string) error { return t.phone.Disconnect(cleanCtx, g) },
			func(taf string) error {
				c, err := testaccount.Sweep(cleanCtx, t.kiff, []string{taf})
				if err == nil && c.Left() > 0 {
					err = errors.New("it's still on the account")
				}
				return err
			})
		n, err := sweepWeekly(cleanCtx, t, f.Name)
		fmt.Fprintf(stdout, "  cleanup: %d of this run's taken away; %d by name, this run's or earlier ones' (%v)\n", mine, n, err)
	}()

	fmt.Fprintf(stdout, "== weekly proof, %s family\n", f.Name)
	// The weekly results go to the job's summary, never to KIFF's proof ingest:
	// a weekly failure can be Coach's wording, not the app's.
	defer func() { writeSummary(env("GITHUB_STEP_SUMMARY"), weeklySummary(f, r, retErr)) }()
	r, err = driveCoach(ctx, chat, &liveWeeklyCloud{t: t, coach: chat.coach}, liveWeeklyPhone{t: t}, f, t.tenant, pollWait(5*time.Second))
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

// weeklyAccount is the test account as a weekly run reaches it: the owner's
// phone and session from the kiff-test environment, pinned to the proof's
// account (the daily proof's pin), and Coach's chat on tafs cloud. keyEnv is
// the sandbox key the run's app takes, or "" for an app connected by a
// sign-in.
func weeklyAccount(ctx context.Context, stdout *redacting, env func(string) string, keyEnv string) (*test, *liveWeeklyChat, error) {
	cfg := config{log: stdout, phone: env("PROOF_PHONE_KEY"), session: env("PROOF_KIFF_SESSION"), setupKey: env("E2E_SETUP_KEY")}
	cfg.planque = firstOf(env("E2E_PLANQUE"), "https://planque.kiff.dev")
	cfg.cloud = firstOf(env("E2E_TAFS_CLOUD"), "https://cloud.tafs.kiff.dev")
	tenant := env("PROOF_TENANT")
	need := map[string]string{"PROOF_TENANT": tenant, "PROOF_PHONE_KEY": cfg.phone, "PROOF_KIFF_SESSION": cfg.session}
	vendorKey := ""
	if keyEnv != "" {
		vendorKey = env(keyEnv)
		need[keyEnv] = vendorKey
	}
	for k, v := range need {
		if v == "" {
			return nil, nil, fmt.Errorf("%s is required (from the kiff-test environment)", k)
		}
	}
	stdout.hide(vendorKey, "<the sandbox's key>")
	stdout.hide(cfg.session, "<the session>")
	hc := &http.Client{Timeout: 60 * time.Second}
	t := &test{cfg: cfg, out: stdout, hc: hc, run: time.Now().UTC().Format("0102-150405") + "-" + firstOf(env("GITHUB_RUN_ATTEMPT"), "1")}
	pk, err := testaccount.ParsePhone(cfg.phone)
	if err != nil {
		return nil, nil, err
	}
	t.pk = pk
	me, err := testaccount.KIFFSession(kiff.APIURL, cfg.session, "", pk, hc).Do(ctx, http.MethodGet, "/v1/me", nil)
	if err != nil || me.Err() != nil {
		return nil, nil, fmt.Errorf("KIFF didn't take the proof phone's session: %v %v", err, me.Err())
	}
	t.tenant, _ = me.Body["tenant_id"].(string)
	stdout.hide(t.tenant, "<the account>")
	fp, err := keyFingerprint(&pk.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	if err := proofPin(t.tenant, tenant, fp); err != nil {
		return nil, nil, fmt.Errorf("the weekly proof did nothing: %w", err)
	}
	t.kiff = testaccount.KIFFSession(kiff.APIURL, cfg.session, t.tenant, pk, hc)
	t.phone = testaccount.NewPhone(pk, cfg.planque, t.tenant, hc)
	t.phone.VendorKey = vendorKey
	if t.phone.Device, err = testaccount.ThisDevice(ctx, t.kiff); err != nil {
		return nil, nil, err
	}
	chat := &liveWeeklyChat{t: t}
	if chat.coach, err = coachOf(ctx, chat.cloud()); err != nil {
		// Never start a Coach from the proof.
		return nil, nil, err
	}
	return t, chat, nil
}

// weeklyCleanup takes away what the run recorded: each grant it approved
// (only grants the setup agent asked for are recorded) is disconnected, and
// the taf Coach handed over is archived only if the run made it, that is it
// wasn't on the account (before) when the run began. It returns how many it
// took away.
func weeklyCleanup(r weeklyResult, before map[string]bool, log io.Writer, disconnect, archive func(string) error) int {
	mine := 0
	for _, g := range r.Grants {
		if err := disconnect(g); err != nil {
			fmt.Fprintf(log, "  cleanup: a grant this run asked for wasn't disconnected (%v)\n", err)
			continue
		}
		mine++
	}
	switch {
	case r.Taf == "":
	case before[r.Taf]:
		fmt.Fprintln(log, "  cleanup: the taf Coach handed over was on the account before this run, so it's left as it is")
	default:
		if err := archive(r.Taf); err != nil {
			fmt.Fprintf(log, "  cleanup: the taf this run made wasn't archived (%v)\n", err)
		} else {
			mine++
		}
	}
	return mine
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

// weeklySummary is a family's weekly result for the job's summary: the
// outcome, the turns and the spend, and every tap with what it agreed to. No
// id, key or vendor data.
func weeklySummary(f weeklyFamily, r weeklyResult, err error) string {
	var b strings.Builder
	outcome := "PASS"
	if err != nil {
		outcome = "FAIL: " + scrubWhy(err.Error())
	}
	fmt.Fprintf(&b, "### Weekly proof, %s family: %s\n\n", f.Name, outcome)
	fmt.Fprintf(&b, "- %d turns, US$%.2f of US$%.2f\n- %d approvals through the component route\n", r.Turns, float64(r.Spent)/1e6, f.CapUSD, len(r.Approvals))
	for _, t := range r.Taps {
		if t.Agrees != "" {
			fmt.Fprintf(&b, "- tapped %q, agreeing to a rules notice shown in the chat\n", t.Words)
		} else {
			fmt.Fprintf(&b, "- tapped %q\n", t.Words)
		}
	}
	return b.String()
}

// writeSummary appends to the job's summary file, when there is one.
func writeSummary(path, text string) { _ = appendFile(path, text+"\n") }

// appendFile appends text to path, when there is one: the job's summary or
// the step's outputs, which GitHub reads after the step.
func appendFile(path, text string) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
