package main

// The integration proofs (-proof <app>): each app that connects today is run
// as a person's taf would use it, with no model, on the test account, and the
// run's summary is posted to KIFF, which keeps each app's state.
//
// A proof drives the path Coach's taf takes, step by step, deterministically:
//
//  1. connects: the setup agent asks planque for the app's catalog entry for
//     a new agent named proof-<app>-<run>, with the proof's tools, and the
//     phone gives the app's sandbox key.
//  2. words: the setup agent submits the taf's Cards, by the grant's real
//     tool names, and tafs cloud's check (the one Approve makes before it
//     asks for a signature) must pass.
//  3. approval_applied: the phone signs the grant and the draft is confirmed
//     in KIFF; the taf's key is claimed.
//  4. reads: the taf reads through KIFF's gateway. Where the sandbox is
//     seeded, the read must list at least what the fixture says; the count
//     is recorded either way.
//  5. card_limit_held, hold_raised, writes: a call the Card doesn't let
//     through. A read-only app's is a tool on the grant that no Card covers,
//     which KIFF must refuse. An app with writes holds every write for the
//     owner (a count total of 0), and the phone refuses it: the hold must end
//     refused, and where the sandbox can be read back, nothing reached it
//     (vendor_received).
//  6. The summary goes to KIFF (POST /v1/ops/proofs) with the run's GitHub
//     OIDC token. Then the grant is disconnected and the agent archived, and
//     so is whatever an earlier proof of the app left (proof-<app>-…).
//
// Nothing the vendor holds, no id and no key reaches the log or the summary.

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// proofApp is one app a proof runs, as its sandbox account was set up.
type proofApp struct {
	Name  string // the app, as KIFF keeps its state: "pipedrive"
	Entry string // planque's catalog entry, name@rev
	// KeyEnv names the environment variable with the sandbox's key, which the
	// phone gives planque. Never read by anyone: the workflow sets it from
	// the kiff-test environment.
	KeyEnv string
	// Read is the tool the proof reads with, and ReadArgs its arguments.
	Read     string
	ReadArgs map[string]any
	// Seeded is the least the read lists when the sandbox has data: 0 for a
	// sandbox not seeded, whose empty list is no defect.
	Seeded int
	// Pick is the one item of the read the proof may act on, by name (Moneybird's
	// test books), never another; "" for none. PickArg is the argument the
	// picked item's id goes in for the held call.
	Pick    string
	PickArg string
	// Held is the call the Card must not let through, and HeldArgs its
	// arguments. Write: it's a write, held for the owner and refused;
	// otherwise a read on the grant that no Card covers, refused by KIFF.
	Held     string
	HeldArgs map[string]any
	Write    bool
	// Unchanged is the read that lists what the held write would add: its
	// count is read before the write and must be the same after the refusal
	// (vendor_received); "" when the sandbox can't be read back.
	Unchanged string
	// Expires is when the sandbox account ends (a trial): past it, a failing
	// proof says "account expired", not that the app is broken.
	Expires time.Time
}

// proofApps are the apps with a sandbox account today (integrations-prover's
// fixtures). An app with no sandbox yet isn't proved, never proved empty.
var proofApps = map[string]proofApp{
	"typeform": {
		Name: "typeform", Entry: "typeform-key@1", KeyEnv: "PROOF_TYPEFORM_KEY",
		Read: "typeform_list_forms", ReadArgs: map[string]any{}, Seeded: 1,
		Held: "typeform_list_responses", HeldArgs: map[string]any{"form_id": "proof"},
	},
	"moneybird": {
		Name: "moneybird", Entry: "moneybird-key@1", KeyEnv: "PROOF_MONEYBIRD_KEY",
		Read: "moneybird_list_administrations", ReadArgs: map[string]any{}, Seeded: 1,
		// Only the test books: the account also holds the company's own.
		Pick: "KIFF Test Books", PickArg: "administration_id",
		Held: "moneybird_list_contacts", HeldArgs: map[string]any{},
	},
	"pipedrive": {
		Name: "pipedrive", Entry: "pipedrive-key@1", KeyEnv: "PROOF_PIPEDRIVE_KEY",
		Read: "pipedrive_list_persons", ReadArgs: map[string]any{}, Seeded: 0,
		Held: "pipedrive_add_activity", HeldArgs: map[string]any{"subject": "KIFF proof: held, never sent", "type": "task"},
		Write: true, Unchanged: "pipedrive_list_activities",
		// A 14-day trial.
		Expires: time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC),
	},
}

// proofAppNames are the apps -proof takes, in order.
func proofAppNames() []string {
	names := make([]string, 0, len(proofApps))
	for n := range proofApps {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// proofGrant is the grant a proof connected, as planque named its tools for
// the taf: each catalog tool's name with the grant's suffix.
type proofGrant struct {
	ID    string
	Agent string
	// Names is the taf's name of each catalog tool, by the catalog's name.
	Names map[string]string
}

// proofCall is what came of one call through KIFF's gateway.
type proofCall struct {
	State string // the gateway's: forwarded, held, refused, failed, …
	// Items are the names of what a list read returned (for Pick), and Count
	// how many; -1 when the result isn't a list.
	Items []proofItem
	Count int
	Hold  string // the held call's exception, when State is held
	Text  string // KIFF's or the vendor's words when it wasn't forwarded; never logged
}

// proofItem is one item a read listed: its id and name, kept in memory only.
type proofItem struct{ ID, Name string }

// proofSteps are what a proof does to the account, the vendor's sandbox and
// KIFF: the live ones (proof_live.go), or fakes in tests.
type proofSteps interface {
	// Pin stops the proof before anything else unless it runs on the test
	// account, as the proof phone's key: never on someone's own account.
	Pin(ctx context.Context) error
	// Sweep disconnects the grants and archives the agents earlier proofs of
	// the app left: those named proof-<app>-…, and nothing else.
	Sweep(ctx context.Context, app proofApp) (int, error)
	// Connect asks for the app's grant for a new agent named name, with tools,
	// and gives the sandbox's key on the phone. The grant waits for its Card.
	Connect(ctx context.Context, app proofApp, name string, tools []string) (proofGrant, error)
	// Draft submits the taf's Cards and asks tafs cloud's check, as Approve
	// does before it asks for a signature: ok, or its code.
	Draft(ctx context.Context, g proofGrant, label string, cards []map[string]any) (draft string, ok bool, code string, err error)
	// Approve signs the grant on the phone, confirms the draft in KIFF and
	// claims the taf's key.
	Approve(ctx context.Context, g proofGrant, draft string) error
	// Call calls the taf's tool through KIFF's gateway.
	Call(ctx context.Context, g proofGrant, tool string, args map[string]any) (proofCall, error)
	// Refuse refuses a held call on the phone, signed, and returns the hold's
	// status after.
	Refuse(ctx context.Context, hold string) (string, error)
	// Cleanup disconnects the grant and archives the agent.
	Cleanup(ctx context.Context, g proofGrant) error
}

// proofResult is a proof's checks, in the order they ran, and its timings.
type proofResult struct {
	Checks  []proofCheck
	Outcome string // pass, fail or error
	// Timings are each step's, for p50 and p95.
	Timings []time.Duration
	// Error is the runner's own failure (outcome error), said in the log only.
	Error string
	// Expired: the sandbox account is past its end, and only what that
	// explains failed.
	Expired bool
	// Unpinned: the proof wasn't on the test account as the proof key, so
	// it did nothing and has nothing to post.
	Unpinned bool
}

type proofCheck struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Why  string `json:"why,omitempty"`
}

// The checks KIFF takes, by name.
const (
	checkConnects        = "connects"
	checkReads           = "reads"
	checkWrites          = "writes"
	checkCardLimitHeld   = "card_limit_held"
	checkHoldRaised      = "hold_raised"
	checkApprovalApplied = "approval_applied"
	checkVendorReceived  = "vendor_received"
	checkWords           = "words"
)

// proofChecks are the checks an app's proof has, each sent: one that never
// ran goes as not ok.
func proofChecks(app proofApp) []string {
	names := []string{checkConnects, checkWords, checkApprovalApplied, checkReads, checkCardLimitHeld}
	if app.Write {
		names = append(names, checkHoldRaised, checkWrites)
	}
	if app.Unchanged != "" {
		names = append(names, checkVendorReceived)
	}
	return names
}

// proofAgentPrefix is the start of every agent a proof of app makes: the
// sweep takes these, and only these.
func proofAgentPrefix(app string) string { return "proof-" + app + "-" }

// proofSwept reports whether an agent is one an earlier proof of app left:
// named proof-<app>-…, never Setup's agent, the Eval's or a person's taf.
func proofSwept(app, agent, setup string) bool {
	return agent != "" && agent != setup && strings.HasPrefix(agent, proofAgentPrefix(app))
}

// proofTools are the catalog tools a proof's grant asks for: its read, the
// held call and the read-back.
func proofTools(app proofApp) []string {
	tools := []string{app.Read}
	for _, t := range []string{app.Held, app.Unchanged} {
		if t != "" && !slices.Contains(tools, t) {
			tools = append(tools, t)
		}
	}
	return tools
}

// proofCards are the taf's Cards, by the grant's real tool names: the reads
// a call count generous enough never to get in the way, and a write a count
// total of 0, which asks the owner about every call. A read-only app's held
// call is on no Card, so KIFF refuses it.
func proofCards(app proofApp, g proofGrant) ([]map[string]any, error) {
	name := func(catalog string) (string, error) {
		n := g.Names[catalog]
		if n == "" {
			return "", fmt.Errorf("the grant has no tool for %s", catalog)
		}
		return n, nil
	}
	var reads []map[string]any
	for _, t := range []string{app.Read, app.Unchanged} {
		if t == "" || slices.ContainsFunc(reads, func(r map[string]any) bool { return r["catalog"] == t }) {
			continue
		}
		n, err := name(t)
		if err != nil {
			return nil, err
		}
		reads = append(reads, map[string]any{"catalog": t, "action": n})
	}
	grants := make([]map[string]any, len(reads))
	for i, r := range reads {
		grants[i] = map[string]any{"action": r["action"]}
	}
	cards := []map[string]any{{
		"id": g.Agent + "-reads", "subject": g.Agent, "domain": "gateway", "grants": grants,
		"aggregates": []map[string]any{{"quantity": "count", "limit": 20, "window": "calendar_day"}},
	}}
	if app.Write {
		n, err := name(app.Held)
		if err != nil {
			return nil, err
		}
		cards = append(cards, map[string]any{
			"id": g.Agent + "-writes", "subject": g.Agent, "domain": "gateway",
			"grants":     []map[string]any{{"action": n}},
			"aggregates": []map[string]any{{"quantity": "count", "limit": 0, "window": "lifetime"}},
			"on_exceed":  "owner",
		})
	}
	return cards, nil
}

// runProof runs app's proof with steps, and says what came of each check.
// A step that fails stops the steps after it, whose checks go as not ok.
// Cleanup runs whatever happened.
func runProof(ctx context.Context, app proofApp, run string, steps proofSteps, now func() time.Time) (r proofResult) {
	set := map[string]proofCheck{}
	check := func(name string, ok bool, why string) { set[name] = proofCheck{Name: name, OK: ok, Why: why} }
	timed := func(f func() error) error {
		start := now()
		err := f()
		r.Timings = append(r.Timings, now().Sub(start))
		return err
	}
	finish := func() proofResult {
		for _, name := range proofChecks(app) {
			c, ok := set[name]
			if !ok {
				c = proofCheck{Name: name, OK: false, Why: "not reached"}
			}
			r.Checks = append(r.Checks, c)
		}
		if r.Outcome == "" {
			r.Outcome = "pass"
			for _, c := range r.Checks {
				if !c.OK {
					r.Outcome = "fail"
				}
			}
		}
		if r.Outcome == "fail" && !app.Expires.IsZero() && !now().Before(app.Expires) {
			// The sandbox's trial is over. Only what an ended account
			// explains is labelled, and its reason kept: a limit that didn't
			// hold is never put down to the account.
			r.Expired = true
			for i, c := range r.Checks {
				switch {
				case c.OK || c.Why == "not reached":
				case c.Name == checkConnects || c.Name == checkReads:
					r.Checks[i].Why = cmpJoin(c.Why, "account expired")
				default:
					r.Expired = false
				}
			}
		}
		return r
	}
	broke := func(step string, err error) proofResult {
		// The runner itself broke: not counted for or against the app.
		r.Outcome, r.Error = "error", step+": "+err.Error()
		return finish()
	}

	if err := steps.Pin(ctx); err != nil {
		r.Unpinned = true
		return broke("not the test account and the proof key", err)
	}
	if _, err := steps.Sweep(ctx, app); err != nil {
		return broke("the sweep of earlier proofs", err)
	}
	var g proofGrant
	err := timed(func() error {
		var err error
		g, err = steps.Connect(ctx, app, proofAgentPrefix(app.Name)+run, proofTools(app))
		return err
	})
	if err != nil {
		check(checkConnects, false, "planque didn't connect it: "+whyOf(err))
		return finish()
	}
	defer func() {
		if err := steps.Cleanup(context.WithoutCancel(ctx), g); err != nil {
			r.Error = cmpJoin(r.Error, "cleanup: "+err.Error())
		}
	}()
	check(checkConnects, true, "")

	cards, err := proofCards(app, g)
	if err != nil {
		check(checkWords, false, whyOf(err))
		return finish()
	}
	var draft string
	var ok bool
	var code string
	if err := timed(func() error {
		var err error
		draft, ok, code, err = steps.Draft(ctx, g, "Proof of "+app.Name, cards)
		return err
	}); err != nil {
		check(checkWords, false, "the draft wasn't checked: "+whyOf(err))
		return finish()
	}
	if !ok {
		check(checkWords, false, "tafs cloud's check refused the Cards: "+code)
		return finish()
	}
	check(checkWords, true, "")

	if err := timed(func() error { return steps.Approve(ctx, g, draft) }); err != nil {
		check(checkApprovalApplied, false, whyOf(err))
		return finish()
	}
	check(checkApprovalApplied, true, "")

	var read proofCall
	if err := timed(func() error {
		var err error
		read, err = steps.Call(ctx, g, g.Names[app.Read], app.ReadArgs)
		return err
	}); err != nil || read.State != "forwarded" {
		check(checkReads, false, "the read wasn't forwarded: "+stateOf(read, err))
		return finish()
	}
	readOK, readWhy := readsHeld(app, read)
	check(checkReads, readOK, readWhy)
	if !readOK {
		return finish()
	}
	held := app.HeldArgs
	if app.Pick != "" {
		i := slices.IndexFunc(read.Items, func(it proofItem) bool { return it.Name == app.Pick })
		held = withArg(app.HeldArgs, app.PickArg, read.Items[i].ID)
	}

	before := read
	if app.Unchanged != "" && app.Unchanged != app.Read {
		var err error
		if before, err = steps.Call(ctx, g, g.Names[app.Unchanged], map[string]any{}); err != nil || before.State != "forwarded" {
			check(checkVendorReceived, false, "the read-back before the write wasn't forwarded: "+stateOf(before, err))
			return finish()
		}
	}

	var call proofCall
	if err := timed(func() error {
		var err error
		call, err = steps.Call(ctx, g, g.Names[app.Held], held)
		return err
	}); err != nil {
		check(checkCardLimitHeld, false, "the call outside the Card failed: "+whyOf(err))
		return finish()
	}
	if !app.Write {
		ok := call.State == "refused"
		check(checkCardLimitHeld, ok, ifNot(ok, "a call no Card covers was "+call.State+", not refused"))
		return finish()
	}
	raised := call.State == "held" && call.Hold != ""
	check(checkHoldRaised, raised, ifNot(raised, "the write was "+call.State+", not held for the owner"))
	check(checkCardLimitHeld, call.State != "forwarded", ifNot(call.State != "forwarded", "the write went through, past a Card that asks every call"))
	if !raised {
		return finish()
	}
	var status string
	if err := timed(func() error {
		var err error
		status, err = steps.Refuse(ctx, call.Hold)
		return err
	}); err != nil {
		check(checkWrites, false, "the phone's refusal failed: "+whyOf(err))
		return finish()
	}
	check(checkWrites, status == "refused", ifNot(status == "refused", "the held write ended "+status+", not refused"))
	if app.Unchanged != "" {
		after, err := steps.Call(ctx, g, g.Names[app.Unchanged], map[string]any{})
		same := err == nil && after.State == "forwarded" && after.Count >= 0 && after.Count == before.Count
		why := ""
		if !same {
			why = fmt.Sprintf("the sandbox lists %d after the refusal, %d before", after.Count, before.Count)
		}
		check(checkVendorReceived, same, why)
	}
	return finish()
}

// readsHeld is whether a forwarded read is what the fixture expects: at least
// Seeded items where the sandbox is seeded, and the one item to act on.
func readsHeld(app proofApp, read proofCall) (bool, string) {
	if app.Seeded > 0 && read.Count < app.Seeded {
		return false, fmt.Sprintf("the seeded sandbox listed %d, at least %d expected", max(read.Count, 0), app.Seeded)
	}
	if app.Pick != "" && !slices.ContainsFunc(read.Items, func(it proofItem) bool { return it.Name == app.Pick }) {
		return false, "the test item isn't in the list"
	}
	return true, fmt.Sprintf("listed %d", max(read.Count, 0))
}

func withArg(args map[string]any, k string, v any) map[string]any {
	out := make(map[string]any, len(args)+1)
	for a, x := range args {
		out[a] = x
	}
	out[k] = v
	return out
}

func ifNot(ok bool, why string) string {
	if ok {
		return ""
	}
	return why
}

func stateOf(c proofCall, err error) string {
	if err != nil {
		return whyOf(err)
	}
	return c.State
}

func cmpJoin(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// whyOf is an error as a check's why: one line, short, with nothing that
// looks like an id, a key or an address left in it.
func whyOf(err error) string {
	return scrubWhy(err.Error())
}

var (
	// emailLike and keyLike are what a why must never carry: KIFF refuses a
	// summary with a credential in it, and the summary is shown on a console.
	emailLike = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+`)
	keyLike   = regexp.MustCompile(`\b[A-Za-z0-9_\-]*[0-9][A-Za-z0-9_\-]*[A-Za-z][A-Za-z0-9_\-]{14,}\b|\b[A-Za-z0-9_\-]*[A-Za-z][A-Za-z0-9_\-]*[0-9][A-Za-z0-9_\-]{14,}\b`)
	idLike    = regexp.MustCompile(`\b(?:grt|agt|tc|exc|drf|card)_[A-Za-z0-9]+\b`)
	urlLike   = regexp.MustCompile(`https?://\S+`)
)

// scrubWhy keeps a why to one line of at most 200 characters, with ids,
// keys, addresses and links taken out.
func scrubWhy(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	for _, re := range []*regexp.Regexp{urlLike, emailLike, idLike, keyLike} {
		s = re.ReplaceAllString(s, "<…>")
	}
	if r := []rune(s); len(r) > 200 {
		s = string(r[:199]) + "…"
	}
	return s
}

// proofSummary is a run's summary as KIFF takes it (schema 1).
type proofSummary struct {
	Schema        int             `json:"schema"`
	RunID         string          `json:"run_id"`
	App           string          `json:"app"`
	Job           string          `json:"job"`
	Tenant        string          `json:"tenant"`
	Trigger       string          `json:"trigger"`
	StartedAt     time.Time       `json:"started_at"`
	EndedAt       time.Time       `json:"ended_at"`
	Outcome       string          `json:"outcome"`
	Checks        []proofCheck    `json:"checks"`
	LimitsHeld    bool            `json:"limits_held"`
	P50Ms         int             `json:"p50_ms"`
	P95Ms         int             `json:"p95_ms"`
	RunnerVersion string          `json:"runner_version"`
	EventsKey     string          `json:"events_key"`
	Entry         string          `json:"entry"`
	Rules         json.RawMessage `json:"rules"`
}

// proofRunID is a run's id as KIFF takes it: the GitHub run, its attempt and the app.
var proofRunID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// summaryOf is the summary of app's run r.
func summaryOf(app proofApp, r proofResult, runID, tenant, trigger, version string, started, ended time.Time) proofSummary {
	s := proofSummary{
		Schema: 1, RunID: runID, App: app.Name, Job: "connect, read and hold on " + app.Entry,
		Tenant: tenant, Trigger: trigger, StartedAt: started.UTC(), EndedAt: ended.UTC(),
		Outcome: r.Outcome, Checks: r.Checks, RunnerVersion: version,
		EventsKey: "proofs/" + runID, Entry: app.Entry,
		// No model and no judge: nothing's words judged the run.
		Rules: json.RawMessage(`{"model":"none"}`),
	}
	for _, c := range r.Checks {
		if c.Name == checkCardLimitHeld {
			s.LimitsHeld = c.OK
		}
	}
	s.P50Ms, s.P95Ms = percentile(r.Timings, 50), percentile(r.Timings, 95)
	return s
}

// percentile is the p-th percentile of ds in milliseconds, nearest rank; 0 for none.
func percentile(ds []time.Duration, p int) int {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	i := (p*len(s)+99)/100 - 1
	return int(s[max(i, 0)].Milliseconds())
}

// summaryLeaks is what in a summary looks like a key, an address or an id;
// nil when nothing does. The summary is posted only when it's nil.
func summaryLeaks(s proofSummary) []string {
	var texts []string
	texts = append(texts, s.Job, string(s.Rules))
	for _, c := range s.Checks {
		texts = append(texts, c.Why)
	}
	var found []string
	for _, t := range texts {
		for _, re := range []*regexp.Regexp{emailLike, keyLike, idLike, urlLike} {
			if m := re.FindString(t); m != "" {
				found = append(found, re.String())
			}
		}
	}
	return found
}

// postProof sends the summary to KIFF's ingest with the run's OIDC token. A
// refusal or a timeout is an error, so the job fails: the console never
// keeps an old pass as current because a post was lost.
func postProof(ctx context.Context, hc *http.Client, apiURL, token string, s proofSummary) error {
	if leaks := summaryLeaks(s); len(leaks) > 0 {
		return fmt.Errorf("the summary has text shaped like a key, an address or an id (%d): not posted", len(leaks))
	}
	body, err := json.Marshal(s)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(apiURL, "/")+"/v1/ops/proofs", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("posting the proof: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("KIFF refused the proof: %d %s", resp.StatusCode, scrubWhy(string(b)))
	}
	return nil
}

// oidcToken is this GitHub run's OIDC token for audience, from the runner's
// own request URL and token; never printed.
func oidcToken(ctx context.Context, hc *http.Client, env func(string) string, audience string) (string, error) {
	u, bearer := env("ACTIONS_ID_TOKEN_REQUEST_URL"), env("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if u == "" || bearer == "" {
		return "", errors.New("no GitHub OIDC token here: the job needs id-token: write")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+"&audience="+audience, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "bearer "+bearer)
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Value string `json:"value"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out) != nil || out.Value == "" {
		return "", fmt.Errorf("GitHub gave no OIDC token (%d)", resp.StatusCode)
	}
	return out.Value, nil
}

// printProof writes the run's checks, each on a line, with no ids: the log is public.
func printProof(w io.Writer, app proofApp, r proofResult) {
	fmt.Fprintf(w, "== proof of %s (%s): %s\n", app.Name, app.Entry, r.Outcome)
	for _, c := range r.Checks {
		mark := "ok"
		if !c.OK {
			mark = "FAILED"
		}
		line := fmt.Sprintf("  %-17s %s", c.Name, mark)
		if c.Why != "" {
			line += ": " + c.Why
		}
		fmt.Fprintln(w, line)
	}
	if r.Expired {
		fmt.Fprintln(w, "  the sandbox account has expired: this isn't the app failing")
	}
	if r.Error != "" {
		fmt.Fprintln(w, "  runner: "+scrubWhy(r.Error))
	}
}

// proofKeys are the phone keys a proof may run as, by fingerprint (the
// first 16 hex characters of the SHA-256 of the DER public key, as KIFF
// shows it): the proof key, and the Eval's test phone.
var proofKeys = []string{"68dccba567dafea9", "a98af0aa046b0fdb"}

// keyFingerprint is a public key's fingerprint, as KIFF shows it.
func keyFingerprint(pub *ecdsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])[:16], nil
}

// proofPin refuses a proof that isn't on the test account (want, from the
// kiff-test environment) as one of proofKeys. Neither is said in full: the
// log is public.
func proofPin(tenant, want, fingerprint string) error {
	switch {
	case want == "":
		return errors.New("PROOF_TENANT isn't set: a proof runs only on the test account")
	case tenant != want:
		return errors.New("the session isn't the test account's")
	case !slices.Contains(proofKeys, fingerprint):
		return errors.New("the phone key isn't the proof key")
	}
	return nil
}
