package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/kiff/agents/tool/kiff"
)

// Step 9's people (kiff/tafs#294): a taf shared with the account's Support
// group is used by Maria, an editor, and Luis, a viewer, each signed in with
// their own session and phone key. Each sees only their own use of it: their
// own calls in KIFF's activity, and in Needs you their own holds and those
// they may answer. The admin sees everything. The test makes the taf itself,
// on tafs cloud, so the people's runs carry their own run grants, and removes
// it after.

// someone is one of step 9's people, signed in as -people-signin left them.
type someone struct {
	name, role string
	key        *ecdsa.PrivateKey
	session    string
	kiff       *client // KIFF, as their phone calls it, once the account is known
	cloud      *client // tafs cloud, with their session
	// What they ask the taf for, by charge: a refund inside the Card, and one
	// over its limit for one call, which KIFF holds.
	inside, held string
	// answers is KIFF's word on whether they may answer another's held call:
	// holds.answer in their /v1/me capabilities.
	answers bool
}

// peopleFrom reads step 9's people from the environment; none when neither's
// secrets are set, an error when only some are.
func peopleFrom(env func(string) string, kiffBase, cloudBase string, hc *http.Client) ([]*someone, error) {
	var out []*someone
	missing := 0
	for _, p := range []struct{ name, role string }{{"maria", "editor"}, {"luis", "viewer"}} {
		upper := strings.ToUpper(p.name)
		session, keyText := env("E2E_"+upper+"_SESSION"), env("E2E_"+upper+"_PHONE_KEY")
		if session == "" || keyText == "" {
			missing++
			continue
		}
		key, err := parsePhone(keyText)
		if err != nil {
			return nil, fmt.Errorf("E2E_%s_PHONE_KEY: %w", upper, err)
		}
		out = append(out, &someone{name: p.name, role: p.role, key: key, session: session,
			kiff:  kiffSession(kiffBase, session, "", key, hc),
			cloud: &client{base: strings.TrimRight(cloudBase, "/"), http: hc, auth: bearerAuth(session)}})
	}
	switch {
	case missing == 2:
		return nil, nil
	case missing > 0:
		return nil, errors.New("-people needs both people's sessions and phone keys; run infra/e2e-people-signin.sh")
	}
	return out, nil
}

func bearerAuth(token string) func(*http.Request, []byte) {
	return func(req *http.Request, _ []byte) { req.Header.Set("Authorization", "Bearer "+token) }
}

// peopleChecks is step 9 on the taf Setup made: tafs cloud starts it from
// its draft, it is shared with Support, each person asks it for their own
// refunds, and what KIFF shows each of them is checked.
func (t *test) peopleChecks(ctx context.Context, tafAgent, draftID string) error {
	admin := &client{base: strings.TrimRight(t.cfg.cloud, "/"), http: t.hc, auth: bearerAuth(t.cfg.session)}

	for _, p := range t.people {
		me, err := p.kiff.do(ctx, http.MethodGet, "/v1/me", nil)
		if err != nil || me.err() != nil {
			return fmt.Errorf("KIFF didn't take %s's session (renew it with infra/e2e-people-signin.sh): %v %v", p.name, err, me.err())
		}
		tenant, _ := me.Body["tenant_id"].(string)
		role, _ := me.Body["member_role"].(string)
		if tenant != t.tenant || role != p.role {
			return fmt.Errorf("%s is signed in as %q on another account or role; want %s on the test's", p.name, role, p.role)
		}
		// Who answers a hold follows the account's separation of duties: off, every
		// role holding holds.answer does (admin, editor, approver); on, approvers
		// only. KIFF tells each person on /v1/me, the same rule its hold gate uses.
		separated, _ := me.Body["separation_of_duties"].(bool)
		p.answers = hasCapability(me.Body, "holds.answer")
		if want := mayAnswer(p.role, separated); p.answers != want {
			return fmt.Errorf("KIFF says %s (%s, separation of duties %v) may answer holds: %v; want %v", p.name, p.role, separated, p.answers, want)
		}
		t.logf("%s, separation of duties %v, answers holds: %v", p.name, separated, p.answers)
		p.kiff = kiffSession(kiff.APIURL, p.session, tenant, p.key, t.hc)
		p.inside, p.held = "ch_e2e_"+p.name+"_"+t.run, "ch_e2e_"+p.name+"held_"+t.run
		t.out.hide(p.inside, "<"+p.name+"'s charge>")
		t.out.hide(p.held, "<"+p.name+"'s held charge>")
	}

	// The taf on tafs cloud, from Setup's draft: tafs cloud claims the key.
	rec, err := t.keys.Get(ctx, t.tenant, draftID)
	if err != nil {
		return fmt.Errorf("the taf's draft: %w", err)
	}
	made, err := admin.do(ctx, http.MethodPost, "/v1/tafs", map[string]any{"name": "E2E people " + t.run, "job": "refunds",
		"draft_id": draftID, "claim_secret": rec.Secret, "unscheduled": true})
	if err != nil || made.err() != nil {
		return fmt.Errorf("tafs cloud didn't start the taf: %v %v", err, made.err())
	}
	taf, _ := made.Body["taf"].(map[string]any)
	cloudID, _ := taf["id"].(string)
	if cloudID == "" {
		return fmt.Errorf("tafs cloud started a taf with no id: %s", oneLine(made.Raw))
	}
	t.cloudTafs = append(t.cloudTafs, cloudID)
	t.out.hide(cloudID, "<the shared taf>")
	t.logf("tafs cloud runs the taf from Setup's draft")

	// Shared with Support: a grant on the test's own taf, nothing on the group.
	gs, err := t.kiff.do(ctx, http.MethodGet, "/v1/groups", nil)
	if err != nil || gs.err() != nil {
		return fmt.Errorf("listing the account's groups: %v %v", err, gs.err())
	}
	support := groupNamed(gs.Body, "Support")
	if support == "" {
		return errors.New("the account has no Support group; make it in the app")
	}
	// Earlier runs' shares that outlived their taf (an unshare that failed):
	// Support's grants on agents that are no longer live, which the admin's
	// access leaves out. Removing one takes nothing from anyone.
	acc, err := t.kiff.do(ctx, http.MethodGet, "/v1/me/access", nil)
	if err != nil || acc.err() != nil {
		return fmt.Errorf("reading the admin's access: %v %v", err, acc.err())
	}
	if stale := staleShares(gs.Body, support, acc.Body, tafAgent); len(stale) > 0 {
		for _, a := range stale {
			if err := unshare(ctx, t.kiff, a, support); err != nil {
				return fmt.Errorf("removing an earlier run's share: %w", err)
			}
			t.logf("removed an earlier run's share with Support: %s", a)
		}
		fmt.Fprintf(t.cfg.log, "people  removed %d earlier run(s)' shares with Support\n", len(stale))
	}
	sh, err := t.kiff.do(ctx, http.MethodPost, "/v1/agents/"+url.PathEscape(tafAgent)+"/grants", map[string]string{"group": support})
	if err != nil || sh.err() != nil {
		return fmt.Errorf("sharing the taf with Support: %v %v", err, sh.err())
	}
	t.shared = append(t.shared, [2]string{tafAgent, support})
	t.logf("the taf is shared with Support")

	// Each person asks for a refund inside the Card, and one over its limit.
	for _, p := range t.people {
		for _, ask := range []string{"Refund charge " + p.inside + " for 200 cents.", "Refund charge " + p.held + " for 900 cents."} {
			m, err := p.cloud.do(ctx, http.MethodPost, "/v1/tafs/"+url.PathEscape(cloudID)+"/messages", map[string]any{"text": ask, "new_chat": true})
			if err != nil || m.err() != nil {
				return fmt.Errorf("%s's message to the shared taf: %v %v", p.name, err, m.err())
			}
		}
	}
	t.logf("Maria and Luis each asked the shared taf for a refund inside its Card and one over it")

	// Each held call, once KIFF has it: two holds and two refunds.
	holds, err := t.waitForHolds(ctx, t.kiff, tafAgent, 2)
	if err != nil {
		return err
	}
	if err := t.waitForRefunds(ctx, t.kiff, tafAgent, 2); err != nil {
		return err
	}

	// What each reader sees: their own refunds, and the holds they own or may answer.
	var failures []string
	byCharge := map[string]string{} // a held call's exception, by its charge
	for _, h := range holds {
		byCharge[h.charge] = h.id
	}
	for _, r := range append([]*someone{{name: "the admin", kiff: t.kiff, key: t.pk}}, t.people...) {
		seen, err := heldCharges(ctx, r.kiff, tafAgent)
		if err != nil {
			return fmt.Errorf("Needs you as %s: %w", r.name, err)
		}
		want := wantedHolds(r, t.people)
		if !sameSet(seen, want) {
			failures = append(failures, fmt.Sprintf("Needs you as %s shows the holds of %v; want %v", r.name, ownersOf(seen, t.people), ownersOf(want, t.people)))
		}
		refunds, err := refundCount(ctx, r.kiff, tafAgent)
		if err != nil {
			return fmt.Errorf("the activity as %s: %w", r.name, err)
		}
		if wantN := ownRefunds(r, t.people); refunds != wantN {
			failures = append(failures, fmt.Sprintf("the activity as %s shows %d sent refunds; want %d", r.name, refunds, wantN))
		}
	}

	// Refusals, as KIFF's hold gate takes them: refusing grants nothing, so the
	// person whose run asked may refuse their own, and on another's hold too
	// only if their role answers holds under separation of duties (/v1/me's
	// holds.answer); a viewer refuses none. Approving isn't tried: it would send.
	maria, luis := t.people[0], t.people[1]
	for _, c := range []struct {
		who    *someone
		charge string
		may    bool
	}{
		{luis, luis.held, luis.answers},
		{maria, maria.held, maria.answers},
		{maria, luis.held, maria.answers},
	} {
		status, err := t.answerAs(ctx, c.who.kiff, c.who.key, byCharge[c.charge], "reject")
		if err != nil {
			return err
		}
		if ok := status/100 == 2; ok != c.may {
			failures = append(failures, fmt.Sprintf("%s answering the hold on %s: %d; want it %s", c.who.name, ownerOf(c.charge, t.people), status, map[bool]string{true: "taken", false: "refused"}[c.may]))
		}
	}
	// The admin refuses what's left, so no run waits on the owner.
	for _, h := range holds {
		if status, err := t.answerAs(ctx, t.kiff, t.pk, h.id, "reject"); err != nil {
			return err
		} else if status/100 != 2 && status != http.StatusConflict {
			failures = append(failures, fmt.Sprintf("the admin refusing a hold: %d", status))
		}
	}

	// A person who only uses the taf reads its Doors without who wrote and
	// what (tafs cloud keeps the knocks for those who manage it), and not who
	// it's shared with.
	for _, p := range t.people {
		if p.role != "viewer" {
			continue
		}
		d, err := p.cloud.do(ctx, http.MethodGet, "/v1/tafs/"+url.PathEscape(cloudID)+"/doors", nil)
		if err != nil || d.err() != nil {
			return fmt.Errorf("the shared taf's Doors as %s: %v %v", p.name, err, d.err())
		}
		if knocks, _ := d.Body["knocks"].([]any); len(knocks) > 0 {
			failures = append(failures, fmt.Sprintf("%s reads %d of the shared taf's knocks", p.name, len(knocks)))
		}
		if s, _ := p.kiff.do(ctx, http.MethodGet, "/v1/agents/"+url.PathEscape(tafAgent)+"/grants", nil); s.Status/100 == 2 {
			failures = append(failures, p.name+" reads who the taf is shared with")
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("step 9: %s", strings.Join(failures, "; "))
	}
	t.logf("each person sees only their own use of the shared taf; the admin sees both")
	return nil
}

// hold is a held call of the shared taf: KIFF's exception and its charge.
type hold struct{ id, charge string }

// waitForHolds waits until KIFF holds n calls of agent, read by reader.
func (t *test) waitForHolds(ctx context.Context, reader *client, agent string, n int) ([]hold, error) {
	deadline := time.Now().Add(4 * time.Minute)
	for {
		hs, err := holdsOf(ctx, reader, agent)
		if err != nil {
			return nil, err
		}
		if len(hs) >= n {
			for _, h := range hs {
				t.out.hide(h.id, "<a hold>")
			}
			return hs, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("KIFF held %d of the shared taf's calls in 4 minutes; want %d", len(hs), n)
		}
		time.Sleep(5 * time.Second)
	}
}

// waitForRefunds waits until KIFF records n sent refunds of agent.
func (t *test) waitForRefunds(ctx context.Context, reader *client, agent string, n int) error {
	deadline := time.Now().Add(4 * time.Minute)
	for {
		got, err := refundCount(ctx, reader, agent)
		if err != nil {
			return err
		}
		if got >= n {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("KIFF recorded %d of the shared taf's refunds in 4 minutes; want %d", got, n)
		}
		time.Sleep(5 * time.Second)
	}
}

// holdsOf is agent's calls KIFF holds, as reader may see them in Needs you.
func holdsOf(ctx context.Context, reader *client, agent string) ([]hold, error) {
	a, err := reader.do(ctx, http.MethodGet, "/v1/me/card-exceptions?status=held", nil)
	if err != nil || a.err() != nil {
		return nil, fmt.Errorf("Needs you: %v %v", err, a.err())
	}
	var out []hold
	es, _ := a.Body["exceptions"].([]any)
	for _, e := range es {
		m, _ := e.(map[string]any)
		if m["subject"] != agent {
			continue
		}
		id, _ := m["id"].(string)
		out = append(out, hold{id: id, charge: chargeIn(m)})
	}
	return out, nil
}

// chargeIn is the charge a held call acts on: the e2e's charge among its
// resources or parameters.
func chargeIn(m map[string]any) string {
	for _, field := range []string{"resources", "parameters"} {
		vs, _ := m[field].(map[string]any)
		for _, v := range vs {
			if s, ok := v.(string); ok && strings.HasPrefix(s, "ch_e2e_") {
				return s
			}
		}
	}
	return ""
}

func heldCharges(ctx context.Context, reader *client, agent string) ([]string, error) {
	hs, err := holdsOf(ctx, reader, agent)
	var out []string
	for _, h := range hs {
		out = append(out, h.charge)
	}
	return out, err
}

// refundCount is how many of agent's calls KIFF sent and reader sees: writes
// allowed, not held, not reads.
func refundCount(ctx context.Context, reader *client, agent string) (int, error) {
	a, err := reader.do(ctx, http.MethodGet, "/v1/me/activity?actor="+url.QueryEscape(agent)+"&show=allowed&limit=100", nil)
	if err != nil || a.err() != nil {
		return 0, fmt.Errorf("activity: %v %v", err, a.err())
	}
	n := 0
	calls, _ := a.Body["calls"].([]any)
	for _, c := range calls {
		m, _ := c.(map[string]any)
		if ro, _ := m["read_only"].(bool); ro {
			continue
		}
		if held, _ := m["held"].(bool); held {
			continue
		}
		if m["outcome"] == "allowed" {
			n++
		}
	}
	return n, nil
}

// wantedHolds is the holds r should see in Needs you: everyone's for the
// admin; a person's own, and others' if they may answer them.
func wantedHolds(r *someone, people []*someone) []string {
	var out []string
	for _, p := range people {
		if p == r || r.role == "" || r.answers {
			out = append(out, p.held)
		}
	}
	return out
}

// mayAnswer is who may answer another's held call: with separation of duties
// off, the roles holding holds.answer (admin, editor, approver); on, approvers.
func mayAnswer(role string, separated bool) bool {
	switch role {
	case "approver":
		return true
	case "admin", "editor":
		return !separated
	}
	return false
}

// hasCapability reads a capability off a /v1/me body.
func hasCapability(me map[string]any, name string) bool {
	cs, _ := me["capabilities"].([]any)
	for _, c := range cs {
		if c == name {
			return true
		}
	}
	return false
}

// ownRefunds is how many sent refunds r should see: everyone's for the
// admin, their own for a person.
func ownRefunds(r *someone, people []*someone) int {
	if r.role == "" {
		return len(people)
	}
	return 1
}

func ownerOf(charge string, people []*someone) string {
	for _, p := range people {
		if charge == p.inside || charge == p.held {
			return p.name
		}
	}
	return "nobody"
}

func ownersOf(cs []string, people []*someone) []string {
	out := []string{}
	for _, c := range cs {
		out = append(out, ownerOf(c, people))
	}
	slices.Sort(out)
	return out
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// staleShares is the agents group is granted that the admin's /v1/me/access
// doesn't list: archived or gone, so no one uses them. Unless that access
// lists ours, the run's own live taf, it's no proof of what's live: none.
func staleShares(groups map[string]any, group string, access map[string]any, ours string) []string {
	live, _ := access["agents"].(map[string]any)
	if live[ours] == nil {
		return nil
	}
	gs, _ := groups["groups"].([]any)
	var out []string
	for _, g := range gs {
		m, _ := g.(map[string]any)
		if id, _ := m["id"].(string); id != group {
			continue
		}
		agents, _ := m["agents"].([]any)
		for _, a := range agents {
			if id, _ := a.(string); id != "" && live[id] == nil {
				out = append(out, id)
			}
		}
	}
	return out
}

func groupNamed(body map[string]any, name string) string {
	gs, _ := body["groups"].([]any)
	for _, g := range gs {
		m, _ := g.(map[string]any)
		if n, _ := m["name"].(string); strings.EqualFold(n, name) {
			id, _ := m["id"].(string)
			return id
		}
	}
	return ""
}

// answerAs answers a held call as the person whose session and phone key
// these are, signed as the app signs it, and returns KIFF's status.
func (t *test) answerAs(ctx context.Context, k *client, key *ecdsa.PrivateKey, exception, answer string) (int, error) {
	if exception == "" {
		return 0, errors.New("no held call to answer")
	}
	path := "/v1/me/card-exceptions/" + url.PathEscape(exception)
	e, err := t.kiff.do(ctx, http.MethodGet, path, nil)
	if err != nil || e.err() != nil {
		return 0, fmt.Errorf("reading the held call: %v %v", err, e.err())
	}
	version, _ := e.Body["accounting_version"].(float64)
	signedAt := time.Now().UTC().Format(time.RFC3339)
	sig, err := signAnswer(key, answerMessage(t.tenant, exception, answer, int64(version), signedAt))
	if err != nil {
		return 0, err
	}
	a, err := k.do(ctx, http.MethodPost, path+"/"+answer, map[string]any{"signature": map[string]string{"signed_at": signedAt, "value": sig}})
	if err != nil {
		return 0, err
	}
	return a.Status, nil
}
