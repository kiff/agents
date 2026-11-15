package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// checkRoute is tafs cloud's check of a Card draft before it's signed, the
// route the harness names in its log: the same checks Approve makes on an
// owner's app (kiff/tafs approval.ts checkDraft), with nothing signed.
const checkRoute = "POST /v1/connect/grants/{grant}/check"

// draftChecked is what tafs cloud's check answered: OK with the version it
// checked, or Approve's own answer (the "failed: …" Coach hears from an
// owner's app) and its reason code.
type draftChecked struct {
	OK      bool
	Version string
	Answer  string
	Code    string
	// Redraft: a refusal a new draft mends, which tafs cloud hands Coach as
	// the phone reads the chat, before the card is drawn (kiff/tafs
	// handler.ts precheck): tafs cloud's own list, marked on its answer.
	Redraft bool
}

// checkDraft asks tafs cloud whether Approve would sign draft, at version, for
// grant: the harness confirms drafts in KIFF itself, so without it none of
// Approve's checks ran on an Eval's draft (Eval 43681227913, whose Card named
// another connection's tool and was signed). The grant is read with the
// phone's own signed read of its grants, as the app hands it over.
func (t *test) checkDraft(ctx context.Context, grant, draft, version string) (draftChecked, error) {
	proof, err := t.phone.GrantsProof(ctx)
	if err != nil {
		return draftChecked{}, err
	}
	cloud := &client{Base: strings.TrimRight(t.cfg.cloud, "/"), HTTP: t.hc, Auth: bearerAuth(t.cfg.session)}
	a, err := cloud.DoWith(ctx, http.MethodPost, "/v1/connect/grants/"+url.PathEscape(grant)+"/check",
		map[string]string{"draft": draft, "shown_draft": version}, http.Header{"Planque-Device-Signature": {proof}})
	if err != nil {
		return draftChecked{}, fmt.Errorf("tafs cloud's check of the draft: %w", err)
	}
	if err := a.Err(); err != nil {
		return draftChecked{}, fmt.Errorf("tafs cloud's check of the draft: %w", err)
	}
	return checkedFrom(a.Body)
}

// draftVersion is the version KIFF shows for draft now: the one the check is
// asked at, as an app sends the version it read.
func (t *test) draftVersion(ctx context.Context, draft string) (string, error) {
	d, err := t.kiff.Do(ctx, http.MethodGet, "/v1/me/agent-drafts/"+url.PathEscape(draft), nil)
	if err != nil || d.Err() != nil {
		return "", fmt.Errorf("reading the draft %s: %v %v", draft, err, d.Err())
	}
	version, _ := d.Body["version"].(string)
	return version, nil
}

// checkedFrom reads the check's answer. A read again (its Cards or the Cards
// in force unread, another version than read) isn't a refusal to hand Coach:
// the app would read again, and the harness read the draft just before, so
// it fails the step, with the code.
func checkedFrom(body map[string]any) (draftChecked, error) {
	str := func(k string) string { s, _ := body[k].(string); return s }
	switch str("outcome") {
	case "ok":
		if str("version") == "" {
			return draftChecked{}, errors.New("tafs cloud's check said ok with no version")
		}
		return draftChecked{OK: true, Version: str("version")}, nil
	case "answered":
		if !strings.HasPrefix(str("answer"), "failed: ") && !strings.HasPrefix(str("answer"), "already approved") {
			return draftChecked{}, fmt.Errorf("tafs cloud's check answered %q, not a refusal", str("answer"))
		}
		redraft, _ := body["redraft"].(bool)
		return draftChecked{Answer: str("answer"), Code: str("code"), Redraft: redraft}, nil
	case "read_again":
		return draftChecked{}, fmt.Errorf("tafs cloud's check said to read the draft again (%s)", str("code"))
	}
	return draftChecked{}, fmt.Errorf("tafs cloud's check answered outcome %q", str("outcome"))
}

// grantOf is the grant of this run that agent's Card covers: a version's
// draft is checked against the taf's own connection, as its Card's.
func (t *test) grantOf(ctx context.Context, agent string) (string, error) {
	for _, id := range t.grants {
		g, err := t.phone.Grant(ctx, id)
		if err != nil {
			return "", err
		}
		if g.Request.Agent == agent && g.Status == "connected" {
			return id, nil
		}
	}
	return "", fmt.Errorf("no connected grant of this run is for the draft's taf %q", agent)
}

// confirmChecked is a version's draft confirmed in KIFF once tafs cloud's
// check passes it (as "trial" when as says so): a refusal fails the step with
// Approve's answer, as there's no card of Coach's here to hand it to.
func (t *test) confirmChecked(ctx context.Context, draft string, d map[string]any, as string) (answer, error) {
	version, _ := d["version"].(string)
	agent, _ := d["agent_id"].(string)
	grant, err := t.grantOf(ctx, agent)
	if err != nil {
		return answer{}, err
	}
	checked, err := t.checkDraft(ctx, grant, draft, version)
	if err != nil {
		return answer{}, err
	}
	if !checked.OK {
		return answer{}, fmt.Errorf("tafs cloud's check (%s) refused the version's Card (%s): %s", checkRoute, checked.Code, checked.Answer)
	}
	t.logf("the version's draft passed tafs cloud's check (%s), then confirmed in KIFF", checkRoute)
	body := map[string]string{"version": checked.Version}
	if as != "" {
		body["as"] = as
	}
	return t.kiff.Do(ctx, http.MethodPost, "/v1/me/agent-drafts/"+draft+"/confirm", body)
}

// refusedAnswer is the card's answer to Coach for a draft tafs cloud's check
// refused: Approve's own words, as an owner's app hands them to Coach, so
// Coach drafts again on the same "failed: …".
func refusedAnswer(c component, refused string) string {
	return fmt.Sprintf("[%s %s] %s", c.Type, c.ID, refused)
}

// refusalsLine is the public log's line for the drafts tafs cloud's check
// refused this setup, with their reason codes in order (codes carry no data):
// a first draft refused and a second confirmed would otherwise read as one
// draft signed.
func refusalsLine(codes []string) string {
	line := fmt.Sprintf("setup drafts refused by tafs cloud's check: %d", len(codes))
	if len(codes) > 0 {
		line += " (" + strings.Join(codes, ", ") + ")"
	}
	return line
}
