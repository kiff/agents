// Command e2e is the simulated-owner end-to-end test of the tafs setup chat
// (kiff/agents#91, kiff/tafs#50). A scripted owner asks the setup agent to
// connect a vendor's refunds, answers each component it shows as the app
// would, and ends with a Refunds taf whose refund inside its Card reaches
// the vendor and whose refund outside it doesn't.
//
// The setup agent and the taf run on this repository's runtime, in this
// process. Everything they reach is real: the model, KIFF (api.kiff.dev and
// its gateway, with a test account), planque, and the vendor (every night,
// planque's fake vendor, which speaks Stripe's OAuth and consents on its
// own; by hand, any MCP server whose sign-in a person completes).
//
// It fails if the agent skips an approval, if a key, token or claim secret
// shows up in anything the runs recorded, or if the vendor receives
// anything but the one refund inside the Card.
//
// Environment:
//
//	ANTHROPIC_API_KEY   the model's key
//	E2E_PHONE_KEY       base64 PKCS#8 P-256: the owner's phone, toward KIFF and planque
//	E2E_KIFF_SESSION    the phone's KIFF session on the test account (from -signin; 90 days)
//	E2E_SETUP_KEY       the setup agent's KIFF key (from -signin): an agent with no tools
//	E2E_VENDOR          the vendor's MCP server (the fake vendor's /mcp)
//	E2E_VENDOR_TOKEN    the fake vendor's /fake/calls token (the refund checks need it)
//	E2E_PLANQUE         planque, https://planque.kiff.dev by default
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/chat"
	"github.com/kiff/agents/drafts"
	draftsapi "github.com/kiff/agents/drafts/kiffapi"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/model/anthropic"
	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/runner"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
	tooldrafts "github.com/kiff/agents/tool/drafts"
	"github.com/kiff/agents/tool/kiff"
	toolmemory "github.com/kiff/agents/tool/memory"
	"github.com/kiff/agents/tool/planque"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	out := &redacting{w: os.Stdout}
	if err := run(ctx, os.Args[1:], out, os.Getenv); err != nil {
		fmt.Fprintln(out, "FAIL:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "PASS")
}

// opus is the model both agents run on, at its price.
const opus = "claude-opus-5-5"

var opusPrice = model.Price{Input: 4_000_000, Output: 20_000_000, CacheRead: 200_000, CacheWrite: 5_000_000}

type config struct {
	session, setupKey, vendor, vendorToken, planque string
	phone                                           string
	setupDef                                        string
	maxTurns                                        int
	maxCost                                         int64
	keep                                            bool
	log                                             io.Writer
}

// redacting writes what the test says with the account and the vendor's
// address left out: the nightly log is public (AGENTS.md), and GitHub masks
// a secret only as a whole string.
type redacting struct {
	w     io.Writer
	pairs []string
}

func (r *redacting) hide(value, as string) {
	if value != "" {
		r.pairs = append(r.pairs, value, as)
	}
}

func (r *redacting) Write(p []byte) (int, error) {
	if _, err := io.WriteString(r.w, strings.NewReplacer(r.pairs...).Replace(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

func run(ctx context.Context, args []string, stdout *redacting, env func(string) string) error {
	fs := flag.NewFlagSet("e2e", flag.ContinueOnError)
	cfg := config{log: stdout}
	fs.StringVar(&cfg.setupDef, "setup", "", "the setup agent's definition (JSON, from kiff/tafs' setupdef.ts); empty: the built-in one")
	fs.IntVar(&cfg.maxTurns, "owner-turns", 10, "the most messages the owner sends the setup agent")
	maxCost := fs.Float64("max-cost", 3, "the most the whole test may spend on the model, in US dollars")
	fs.BoolVar(&cfg.keep, "keep", false, "leave what the test set up (grant, taf) in place, for a look afterwards")
	signin := fs.String("signin", "", "once, before the test runs: sign the phone in to KIFF (a person approves in a browser), make the setup agent, "+
		"and write the session's token and the setup agent's key to this file (0600)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg.maxCost = int64(*maxCost * 1e6)
	cfg.phone = env("E2E_PHONE_KEY")
	if *signin != "" {
		key, err := parsePhone(cfg.phone)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(*signin, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		return kiffSignIn(ctx, kiff.APIURL, key, "e2e-setup", &http.Client{Timeout: 30 * time.Second}, f, stdout)
	}
	cfg.session, cfg.setupKey = env("E2E_KIFF_SESSION"), env("E2E_SETUP_KEY")
	cfg.vendor, cfg.vendorToken = env("E2E_VENDOR"), env("E2E_VENDOR_TOKEN")
	if u, err := url.Parse(cfg.vendor); err == nil && u.Host != "" {
		stdout.hide(strings.TrimRight(cfg.vendor, "/"), "<the vendor>/mcp")
		stdout.hide(u.Host, "<the vendor>")
	}
	cfg.planque = env("E2E_PLANQUE")
	if cfg.planque == "" {
		cfg.planque = "https://planque.kiff.dev"
	}
	for name, v := range map[string]string{"ANTHROPIC_API_KEY": env("ANTHROPIC_API_KEY"), "E2E_KIFF_SESSION": cfg.session,
		"E2E_SETUP_KEY": cfg.setupKey, "E2E_PHONE_KEY": cfg.phone, "E2E_VENDOR": cfg.vendor} {
		if v == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	t := &test{cfg: cfg, out: stdout, hc: &http.Client{Timeout: 60 * time.Second}, run: time.Now().UTC().Format("0102-1504")}
	defer t.cleanup()
	return t.scenario(ctx, anthropic.New())
}

// test is one run of the scenario, and what it must clean up.
type test struct {
	cfg    config
	out    *redacting
	hc     *http.Client
	run    string // names this run's agents and keys
	tenant string

	kiff  *client
	phone *phone

	keys     *memory.Drafts // the agents' KIFF keys, by reference, and taf drafts
	sessions *memory.Store
	rn       *runner.Runner
	agents   *memory.Agents

	secrets  []string // every secret value the runs must never record
	grants   []string // planque grants this run connected
	cardIDs  []string // Cards the taf's draft issued
	draftIDs []string
	taf      string // the taf's agent id, once handed over
}

func (t *test) logf(format string, args ...any) { fmt.Fprintf(t.cfg.log, "  "+format+"\n", args...) }

func (t *test) scenario(ctx context.Context, m model.Model) error {
	fmt.Fprintln(t.cfg.log, "== setup")
	pk, err := parsePhone(t.cfg.phone)
	if err != nil {
		return err
	}
	t.secrets = append(t.secrets, t.cfg.session, t.cfg.setupKey)
	probe := kiffSession(kiff.APIURL, t.cfg.session, "", pk, t.hc)
	me, err := probe.do(ctx, http.MethodGet, "/v1/me", nil)
	if err != nil || me.err() != nil {
		return fmt.Errorf("KIFF didn't take the phone's session (it lasts 90 days; renew it with -signin): %v %v", err, me.err())
	}
	t.tenant, _ = me.Body["tenant_id"].(string)
	t.out.hide(t.tenant, "<the account>")
	t.kiff = kiffSession(kiff.APIURL, t.cfg.session, t.tenant, pk, t.hc)
	t.phone = newPhone(pk, t.cfg.planque, t.tenant, t.hc)
	if err := t.enrol(ctx); err != nil {
		return err
	}
	if err := t.clearEarlierRuns(ctx); err != nil {
		return err
	}
	setupKey := t.cfg.setupKey
	t.runtime(m)
	if err := t.keys.Create(ctx, t.tenant, "kiff/setup", setupKey); err != nil {
		return err
	}
	setup, err := t.setupDefinition()
	if err != nil {
		return err
	}
	if err := t.agents.Put(ctx, setup); err != nil {
		return err
	}

	fmt.Fprintln(t.cfg.log, "== the setup chat")
	tafID, draftID, err := t.converse(ctx)
	if err != nil {
		return err
	}

	fmt.Fprintln(t.cfg.log, "== the taf")
	if err := t.useTaf(ctx, tafID, draftID); err != nil {
		return err
	}
	return t.checkRecords(ctx)
}

// enrol makes sure planque has the owner's phone: the first time, the
// phone mints planque's connector key in KIFF (a signed setup call, as tafs
// does) and enrols with it. Afterwards it is enrolled already.
func (t *test) enrol(ctx context.Context) error {
	a, err := t.phone.planque.do(ctx, http.MethodGet, t.phone.accountPath()+"/grants", nil)
	if err != nil {
		return err
	}
	switch {
	case a.Status == http.StatusOK:
		t.logf("the phone is enrolled with planque")
		return nil
	case a.Status != http.StatusForbidden:
		return fmt.Errorf("planque doesn't take the test phone (another phone is enrolled for this account?): %w", a.err())
	}
	k, err := t.kiff.do(ctx, http.MethodPost, "/v1/keys", map[string]any{"label": "planque (e2e)", "roles": []string{"connector"}})
	if err != nil || k.err() != nil {
		return fmt.Errorf("minting planque's connector key: %v %v", err, k.err())
	}
	connector, _ := k.Body["plaintext"].(string)
	t.secrets = append(t.secrets, connector)
	a, err = t.phone.planque.do(ctx, http.MethodPost, t.phone.accountPath()+"/enrolment",
		map[string]string{"device_key": t.phone.publicKey(), "connector_key": connector})
	if err != nil || a.err() != nil {
		return fmt.Errorf("enrolling the phone with planque: %v %v", err, a.err())
	}
	t.logf("the phone enrolled with planque, with a connector key it minted")
	return nil
}

// runtime is this repository's runtime, in memory, with the setup agent's
// tools: KIFF's gateway, memory, show, planque and drafts.
func (t *test) runtime(m model.Model) {
	t.keys, t.sessions, t.agents = memory.NewDrafts(), memory.New(), memory.NewAgents()
	claims := &drafts.Service{Store: t.keys, Keys: t.keys, KIFF: draftsapi.New(kiff.APIURL, t.hc)}
	t.rn = &runner.Runner{Runs: memory.NewRuns(), Sessions: t.sessions, Collect: kiff.Collect, Builder: &registry.Builder{
		Agents: t.agents, Secrets: keyring{t.keys}, Model: m, Gateways: []string{kiff.URL},
		Connect: func(ctx context.Context, gateway, key string) ([]tool.Tool, error) {
			return kiff.New(gateway, key, nil).Tools(ctx)
		},
		Own:      func(tenant, agent string) []tool.Tool { return []tool.Tool{toolmemory.New(t.sessions, tenant, agent)} },
		Planques: []string{strings.TrimRight(t.cfg.planque, "/")},
		Planque:  func(base, tenant, key string) tool.Tool { return planque.New(base, tenant, key, t.hc) },
		Drafts:   func(tenant, key string) tool.Tool { return tooldrafts.New(claims, tenant, key) },
	}}
}

// keyring reads the agents' keys from the test's store.
type keyring struct{ d *memory.Drafts }

func (k keyring) Key(ctx context.Context, tenant, ref string) (string, error) {
	key, ok, err := k.d.Key(ctx, tenant, ref)
	if err == nil && !ok {
		err = errors.New("no key under " + ref)
	}
	return key, err
}

// clearEarlierRuns disconnects what an earlier run left connected: tool
// names are the account's, so a stale grant would block this one's.
func (t *test) clearEarlierRuns(ctx context.Context) error {
	gs, err := t.phone.grants(ctx)
	if err != nil {
		return err
	}
	for _, g := range gs {
		if g.Status == "connected" || g.Status == "signed_in" {
			if err := t.phone.disconnect(ctx, g.ID); err != nil {
				return fmt.Errorf("disconnecting an earlier run's grant %s: %w", g.ID, err)
			}
			t.logf("disconnected an earlier run's grant %s", g.ID)
		}
	}
	return nil
}

// setupDefinition is the setup agent: from kiff/tafs' setupdef.ts when
// -setup names its JSON, otherwise the built-in instructions below. Either
// way the test sets what it controls: tenant, id, key, gateway, planque,
// tools and cost cap.
func (t *test) setupDefinition() (registry.Definition, error) {
	d := registry.Definition{Instructions: setupInstructions, Model: opus, Price: opusPrice, MaxTokens: 8192, Effort: "high",
		MaxTurns: 30, History: 10, Keep: session.DefaultPolicy(24 * time.Hour)}
	if t.cfg.setupDef != "" {
		b, err := os.ReadFile(t.cfg.setupDef)
		if err != nil {
			return d, err
		}
		if err := json.Unmarshal(b, &d); err != nil {
			return d, fmt.Errorf("-setup: %w", err)
		}
	}
	d.Tenant, d.ID, d.KeyRef, d.Gateway = t.tenant, "setup", "kiff/setup", kiff.URL
	d.Planque, d.Show, d.Drafts, d.Workspace = strings.TrimRight(t.cfg.planque, "/"), true, true, false
	d.MaxCost = t.cfg.maxCost
	return d, d.Check()
}

// setupInstructions are the setup agent's when kiff/tafs' aren't given.
const setupInstructions = `You set up tafs: agents that do one job for a business owner, each within a KIFF Card.

The owner tells you what to connect. You:
1. Show your plan as steps (show, type steps) and keep it up to date.
2. Ask planque for access to the vendor's account (planque, action request) for the taf's KIFF agent id. Choose a new agent id: lowercase letters, digits and dashes. Grant only the tools the job needs; name the tool that moves money's amount argument.
3. Show an approval with the grant (show, type approval, grant). Wait for the owner's answer.
4. When the owner says it is approved, check the grant (planque, action status). Go on only if it is connected.
5. Submit the taf's KIFF draft (kiff_draft, action submit): its agent id, label, and one Card on the gateway domain:
   {"id": "<agent id>-card", "subject": "<agent id>", "domain": "gateway",
    "grants": [{"action": "<tool name>", "per_action_max": [{"quantity": "sum(<amount argument>)", "max": <the owner's limit>}]}],
    "aggregates": [{"quantity": "sum(<amount argument>)", "limit": <ten times the owner's limit>, "window": "calendar_day"}],
    "on_exceed": "refuse"}
6. Show an approval with the grant and the draft (show, type approval, grant and draft). Wait for the owner's answer.
7. Show the result (show, type result) with the taf's agent id and name, and the tools it can use.

A component grants nothing: never take the owner's word for an approval without checking planque. Never ask for a key, token or password. You can't call the account's tools yourself.`

// component is one component the setup agent showed.
type component struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Grant string `json:"grant"`
	Draft string `json:"draft"`
	Taf   *struct {
		Agent string `json:"agent"`
		Name  string `json:"name"`
	} `json:"taf"`
	Options []struct {
		Value string `json:"value"`
	} `json:"options"`
}

// converse is the owner in the setup chat: one message, then an answer to
// each component the agent shows, until it shows the result.
func (t *test) converse(ctx context.Context) (tafID, draftID string, err error) {
	ch := &chat.Chat{Runner: t.rn, Tenant: t.tenant, Agent: "setup"}
	msg := fmt.Sprintf("Handle refunds from my payments account. Its MCP server is %s. "+
		"Refund with its create_refund tool (charge, and amount in cents). Each refund may be at most 500 cents; anything above must be refused. "+
		"Call the taf Refunds, with agent id refunds-%s.", t.cfg.vendor, t.run)
	answered := map[string]string{}
	approvedGrant := ""
	for turn := 0; turn < t.cfg.maxTurns; turn++ {
		t.logf("owner: %s", msg)
		r, err := ch.Send(ctx, msg, nil)
		if err != nil {
			return "", "", fmt.Errorf("the setup agent's run: %w", err)
		}
		if r.Status != "COMPLETED" && r.Status != "PAUSED" {
			return "", "", fmt.Errorf("the setup agent's run ended %s", r.Status)
		}
		shown, text, err := t.latest(ctx, ch.Session, r.ID)
		if err != nil {
			return "", "", err
		}
		if text != "" {
			t.logf("agent: %s", oneLine(text))
		}
		msg = ""
		for _, c := range shown {
			key := c.Type + "/" + c.ID + "/" + c.Grant + "/" + c.Draft
			if answered[key] != "" {
				continue
			}
			switch c.Type {
			case "confirm":
				if len(c.Options) > 0 {
					msg = fmt.Sprintf("[confirm %s] %s", c.ID, c.Options[0].Value)
				}
			case "approval", "sign_in":
				if err := t.approve(ctx, c, &approvedGrant); err != nil {
					return "", "", err
				}
				if c.Draft != "" {
					draftID = c.Draft
				}
				msg = fmt.Sprintf("[%s %s] approved, signed in", c.Type, c.ID)
			case "result":
				if c.Taf == nil || draftID == "" {
					return "", "", errors.New("the agent showed a result without a confirmed draft behind it: it skipped an approval")
				}
				return c.Taf.Agent, draftID, nil
			}
			answered[key] = msg
		}
		if msg == "" {
			msg = "Yes, go ahead."
		}
	}
	return "", "", fmt.Errorf("no result after %d owner messages", t.cfg.maxTurns)
}

// approve is the owner's answer to an approval: on the phone, the grant
// (approve, sign in, confirm) unless it is already connected; in KIFF, the
// draft.
func (t *test) approve(ctx context.Context, c component, approved *string) error {
	if c.Grant == "" {
		return fmt.Errorf("an %s with no grant", c.Type)
	}
	g, err := t.phone.grant(ctx, c.Grant)
	if err != nil {
		return err
	}
	t.logf("the owner reads: %s", oneLine(g.Text))
	if g.Status != "connected" {
		if err := t.phone.approveAndSignIn(ctx, g); err != nil {
			return fmt.Errorf("the grant %s: %w", g.ID, err)
		}
		t.grants = append(t.grants, g.ID)
		*approved = g.ID
		t.logf("grant %s approved on the phone, signed in at the vendor, confirmed: connected", g.ID)
	}
	if c.Draft == "" {
		return nil
	}
	d, err := t.kiff.do(ctx, http.MethodGet, "/v1/me/agent-drafts/"+c.Draft, nil)
	if err != nil || d.err() != nil {
		return fmt.Errorf("reading the draft %s: %v %v", c.Draft, err, d.err())
	}
	version, _ := d.Body["version"].(string)
	d, err = t.kiff.do(ctx, http.MethodPost, "/v1/me/agent-drafts/"+c.Draft+"/confirm", map[string]string{"version": version})
	if err != nil || d.err() != nil {
		return fmt.Errorf("confirming the draft %s: %v %v", c.Draft, err, d.err())
	}
	t.draftIDs = append(t.draftIDs, c.Draft)
	if ids, ok := d.Body["card_ids"].([]any); ok {
		for _, id := range ids {
			if s, ok := id.(string); ok {
				t.cardIDs = append(t.cardIDs, s)
			}
		}
	}
	t.logf("draft %s confirmed in KIFF: Cards %v", c.Draft, t.cardIDs)
	return nil
}

// latest is what the run shows and says: its components, the latest of
// each id, and its last message.
func (t *test) latest(ctx context.Context, sessionID, runID string) ([]component, string, error) {
	_, events, err := t.sessions.Read(ctx, t.tenant, sessionID)
	if err != nil {
		return nil, "", err
	}
	var shown []component
	text := ""
	for _, e := range events {
		if e.Run != runID {
			continue
		}
		switch e.Kind {
		case session.Component:
			var c component
			if json.Unmarshal(e.Body, &c) == nil {
				shown = slices.DeleteFunc(shown, func(o component) bool { return o.ID == c.ID })
				shown = append(shown, c)
			}
		case session.AgentMessage, session.Report:
			var b agent.TextBody
			if json.Unmarshal(e.Body, &b) == nil {
				text = b.Text
			}
		}
	}
	return shown, text, nil
}

// useTaf is tafs cloud and the owner after the hand-over: the taf's key is
// claimed into the agents' keys, its definition stored, and the owner asks
// it for one refund inside its Card and one outside.
func (t *test) useTaf(ctx context.Context, tafID, draftID string) error {
	t.taf = tafID
	claims := &drafts.Service{Store: t.keys, Keys: t.keys, KIFF: draftsapi.New(kiff.APIURL, t.hc)}
	rec, _ := t.keys.Get(ctx, t.tenant, draftID)
	t.secrets = append(t.secrets, rec.Secret)
	agentID, err := claims.Claim(ctx, t.tenant, draftID, "kiff/taf")
	if err != nil {
		return fmt.Errorf("claiming the taf's key: %w", err)
	}
	if agentID != tafID {
		return fmt.Errorf("the draft's agent is %s, the result names %s", agentID, tafID)
	}
	key, _, _ := t.keys.Key(ctx, t.tenant, "kiff/taf")
	t.secrets = append(t.secrets, key)
	t.logf("the taf's key claimed into the agents' keys (never shown to anyone)")
	taf := registry.Definition{Tenant: t.tenant, ID: tafID, Model: opus, Price: opusPrice, MaxTokens: 4096, Effort: "medium",
		Instructions: "You are Refunds. When the owner asks for a refund, make it with the refund tool, then say what happened in one sentence. " +
			"If the tool refuses, say so; don't retry another way.",
		Gateway: kiff.URL, KeyRef: "kiff/taf", Keep: session.DefaultPolicy(24 * time.Hour), MaxCost: t.cfg.maxCost / 4, MaxTurns: 6, History: 2}
	if err := t.agents.Put(ctx, taf); err != nil {
		return err
	}
	// This run's charges, so only its own calls count: the vendor keeps
	// every run's for a week.
	in, out := "ch_e2e_in_"+t.run, "ch_e2e_out_"+t.run
	for _, ask := range []string{
		"Refund charge " + in + " for 300 cents.",
		"Refund charge " + out + " for 90000 cents.",
	} {
		ch := &chat.Chat{Runner: t.rn, Tenant: t.tenant, Agent: tafID}
		r, err := ch.Send(ctx, ask, nil)
		if err != nil {
			return fmt.Errorf("the taf's run: %w", err)
		}
		_, text, _ := t.latest(ctx, ch.Session, r.ID)
		t.logf("owner: %s → taf: %s", ask, oneLine(text))
	}
	calls, err := t.vendorCalls(ctx)
	if err != nil {
		return err
	}
	var got []string
	for _, c := range calls {
		if strings.Contains(c, in) || strings.Contains(c, out) {
			got = append(got, c)
		}
	}
	t.logf("the vendor received %d call(s) for this run: %v", len(got), got)
	if len(got) != 1 || !strings.Contains(got[0], `"charge":"`+in+`"`) || !strings.Contains(got[0], `"amount":300`) {
		return fmt.Errorf("the vendor received %v; want exactly the refund of %s for 300", got, in)
	}
	return nil
}

// vendorCalls are the tools/call the fake vendor ran, one line each.
func (t *test) vendorCalls(ctx context.Context) ([]string, error) {
	if t.cfg.vendorToken == "" {
		return nil, errors.New("E2E_VENDOR_TOKEN is needed to check what the vendor received")
	}
	c := &client{base: strings.TrimSuffix(strings.TrimRight(t.cfg.vendor, "/"), "/mcp"), http: t.hc,
		auth: func(req *http.Request, _ []byte) { req.Header.Set("Authorization", "Bearer "+t.cfg.vendorToken) }}
	a, err := c.do(ctx, http.MethodGet, "/fake/calls", nil)
	if err != nil || a.err() != nil {
		return nil, fmt.Errorf("reading the vendor's calls: %v %v", err, a.err())
	}
	var out struct {
		Calls []struct {
			Tool string         `json:"Tool"`
			Args map[string]any `json:"Args"`
		} `json:"calls"`
	}
	_ = json.Unmarshal([]byte(a.Raw), &out)
	lines := make([]string, len(out.Calls))
	for i, c := range out.Calls {
		b, _ := json.Marshal(c.Args)
		lines[i] = c.Tool + " " + string(b)
	}
	return lines, nil
}

// checkRecords is the promise that no key, token or claim secret reached
// the model: none appears in anything either agent's runs recorded.
func (t *test) checkRecords(ctx context.Context) error {
	var sessions []session.Session
	for _, a := range []string{"setup", t.taf} {
		ss, err := t.sessions.List(ctx, t.tenant, a, 100)
		if err != nil {
			return err
		}
		sessions = append(sessions, ss...)
	}
	for _, s := range sessions {
		_, events, err := t.sessions.Read(ctx, t.tenant, s.ID)
		if err != nil {
			return err
		}
		for _, e := range events {
			body := string(e.Body)
			for _, secret := range t.secrets {
				if secret != "" && strings.Contains(body, secret) {
					return fmt.Errorf("a secret reached %s's %s event", s.Agent, e.Kind)
				}
			}
			if strings.Contains(body, "kiff_claim_") {
				return fmt.Errorf("a claim secret reached %s's %s event", s.Agent, e.Kind)
			}
		}
	}
	fmt.Fprintln(t.cfg.log, "== no key, token or claim secret in any recorded event")
	return nil
}

// cleanup disconnects the grant and revokes the taf's Cards, unless -keep:
// the taf's key then reaches nothing.
func (t *test) cleanup() {
	if t.cfg.keep || t.kiff == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, g := range t.grants {
		if err := t.phone.disconnect(ctx, g); err != nil {
			t.logf("cleanup: disconnecting %s: %v", g, err)
		}
	}
	for _, id := range t.cardIDs {
		if a, err := t.kiff.do(ctx, http.MethodPost, "/v1/me/mandates/"+id+"/revoke", map[string]string{}); err != nil || a.err() != nil {
			t.logf("cleanup: revoking Card %s: %v %v", id, err, a.err())
		}
	}
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 240 {
		s = string(r[:240]) + "…"
	}
	return s
}
