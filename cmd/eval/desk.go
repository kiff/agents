package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/cmd/internal/testaccount"
	"github.com/kiff/agents/eval"
	"github.com/kiff/agents/eval/cases"
	"github.com/kiff/agents/registry"
)

// A desk case (cases.Scenario.Desk, the tickets case) runs as a fresh agent
// for each run, on the test account's fake desk, as agentgabo and kiff-admin
// agreed: its Cards' counts start at zero, and the standing agent's Stripe
// Cards and room are untouched. Setup's key asks for the agent's API-key grant
// to the desk, the test phone gives the desk's key and confirms the agent's
// Cards, and the agent's own key runs the case. After the run, whatever it
// did, the grant is disconnected and the agent archived; its id is in -made
// first, so the workflow's sweep finds an agent a cut-off run left.

// deskEnv is what a desk case needs from the environment: the same secrets as
// the E2E job's desk run.
var deskEnv = []string{"E2E_KIFF_SESSION", "E2E_SETUP_KEY", "E2E_PHONE_KEY", "E2E_VENDOR", "E2E_DESK_KEY"}

// desk is the test account for desk cases.
type desk struct {
	account testaccount.Account
	vendor  string // the fake vendor's desk, its API and description beside it
	made    string // the sweep's list of the agents a run made (E2E_MADE)
	log     io.Writer
}

// newDesk signs the phone in to KIFF and planque as the E2E job does: the
// session's tenant from KIFF, the phone's device id, Setup's key toward
// planque.
func newDesk(ctx context.Context, env func(string) string, kiffAPI string, log io.Writer) (*desk, error) {
	var missing []string
	for _, k := range deskEnv {
		if env(k) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("a desk case needs %s", strings.Join(missing, ", "))
	}
	planque := env("E2E_PLANQUE")
	if planque == "" {
		planque = "https://planque.kiff.dev"
	}
	key, err := testaccount.ParsePhone(env("E2E_PHONE_KEY"))
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Timeout: 60 * time.Second}
	me, err := testaccount.KIFFSession(kiffAPI, env("E2E_KIFF_SESSION"), "", key, hc).Do(ctx, http.MethodGet, "/v1/me", nil)
	if err != nil || me.Err() != nil {
		return nil, fmt.Errorf("KIFF didn't take the phone's session: %v %v", err, me.Err())
	}
	tenant, _ := me.Body["tenant_id"].(string)
	kiffc := testaccount.KIFFSession(kiffAPI, env("E2E_KIFF_SESSION"), tenant, key, hc)
	phone := testaccount.NewPhone(key, planque, tenant, hc)
	if phone.Device, err = testaccount.ThisDevice(ctx, kiffc); err != nil {
		return nil, err
	}
	phone.VendorKey = env("E2E_DESK_KEY")
	return &desk{
		account: testaccount.Account{Phone: phone, KIFF: kiffc, Setup: testaccount.SetupClient(planque, env("E2E_SETUP_KEY"), hc)},
		vendor:  strings.TrimSuffix(strings.TrimRight(env("E2E_VENDOR"), "/"), "/mcp") + "/desk",
		made:    env("E2E_MADE"),
		log:     log,
	}, nil
}

// agentFor makes the fresh agent for one run of s, and the prefix its desk
// tools carry.
func (d *desk) agentFor(ctx context.Context, s cases.Scenario) (testaccount.Agent, string, error) {
	run, err := testaccount.RunID()
	if err != nil {
		return testaccount.Agent{}, "", err
	}
	prefix := "t" + run
	spec := testaccount.AgentSpec{
		ID:    "eval-" + s.Name + "-" + run,
		Label: "Eval: " + s.Name,
		Tools: cases.DeskTools(prefix),
		API: map[string]any{"url": d.vendor + "/openapi.json", "base": d.vendor + "/v1",
			"auth": map[string]any{"in": "header", "name": "X-Desk-Key"}},
		Cards: cases.DeskCards(prefix),
	}
	ag, err := d.account.NewAgent(ctx, spec, func(agent string) error { return testaccount.RecordMade(d.made, agent) })
	if err != nil {
		return ag, "", fmt.Errorf("%s: the run's agent: %w", s.Name, err)
	}
	return ag, prefix, nil
}

// remove takes the run's agent back; a failure is logged, never the run's
// verdict: the workflow's sweep tries again.
func (d *desk) remove(ag testaccount.Agent, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := d.account.Remove(ctx, ag); err != nil {
		fmt.Fprintf(d.log, "eval: %s: the run's agent wasn't all taken back (the sweep tries again): %v\n", name, redactIDs(err.Error()))
	}
}

// deskDisclosure is where the run's agent puts the line saying an AI agent
// wrote it: each reply's body, as Coach names the desk's reply for a taf that
// answers customers.
func deskDisclosure(prefix string) *agent.Disclosure {
	return &agent.Disclosure{Line: "Written by an AI agent for the company.", In: []agent.DisclosureIn{{Tool: prefix + "_reply", Fields: []string{"body"}}}}
}

// runDesk runs one run of a desk case: the agent made, its key used for this
// run alone, the case graded through the run's tool prefix, the agent taken
// back.
func (rt *runtime) runDesk(ctx context.Context, d *desk, s cases.Scenario) (eval.Run, cases.Scenario, error) {
	if d == nil {
		return eval.Run{}, s, errors.New(s.Name + ": a desk case needs the test account (newDesk)")
	}
	ag, prefix, err := d.agentFor(ctx, s)
	if err != nil {
		return eval.Run{}, s, err
	}
	defer d.remove(ag, s.Name)
	standing := rt.kiffKey
	rt.kiffKey, rt.disclose = ag.Key, deskDisclosure(prefix)
	defer func() { rt.kiffKey, rt.disclose = standing, nil }()
	s = s.ForRun(prefix)
	r, err := rt.Run(ctx, s)
	return r, s, err
}

// runtimeKey is the KIFF key the runtime's agent runs with now: the standing
// agent's, or a desk run's own.
type runtimeKey struct{ rt *runtime }

func (k runtimeKey) Key(context.Context, string, string) (string, error) { return k.rt.kiffKey, nil }

var _ registry.Secrets = runtimeKey{}
