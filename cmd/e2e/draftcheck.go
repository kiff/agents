package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
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

// checkedOnRead is whether the owner's app checks an approval card's Card
// draft as it reads the chat, before drawing the card: when the card signs
// the Card (kiff/tafs approval.ts stepFor), a connected app's Card alone, or
// the Card with tools added to an app signed in already (tools_and_cards),
// or with a key (key_and_cards). A first connection signs the connection
// alone: its Card comes on a card of its own, checked then.
func checkedOnRead(g grant) bool {
	switch {
	case g.Status == "connected":
		return true
	case g.Status == "pending":
		return g.Request.Kind == "api_key" || g.SignedIn
	}
	return false
}

// resultsLast is the cards in the order they're answered: a result after the
// others, so an approval shown beside it is answered, and its draft
// confirmed, before the result is read.
func resultsLast(shown []component) []component {
	out := slices.Clone(shown)
	slices.SortStableFunc(out, func(a, b component) int {
		return cmp.Compare(boolInt(a.Type == "result"), boolInt(b.Type == "result"))
	})
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// unconfirmedAnswer is tafs cloud's answer to a result over a draft the owner
// never confirmed, in its words (kiff/tafs handler.ts cardNeverApproved).
func unconfirmedAnswer(c component) string {
	name := "the taf"
	if c.Taf != nil && strings.TrimSpace(c.Taf.Name) != "" {
		name = strings.TrimSpace(c.Taf.Name)
	}
	out, _ := json.Marshal(map[string]string{"failed": fmt.Sprintf("The Card for %s was never approved, so %s isn't set up yet: approve its Card to finish", name, name)})
	return fmt.Sprintf("[%s %s] %s", c.Type, c.ID, out)
}

// unconfirmedLine is the public log's line for the results answered back to
// Coach over a draft this setup never confirmed.
func unconfirmedLine(n int) string {
	return fmt.Sprintf("setup results answered back, their draft not confirmed: %d", n)
}

// noteRead prints in the transcript what a notice or a confirm card says, as
// the owner reads it: Coach's rules notice above its confirm is where a limit
// the owner never gave first shows (Eval 14's Refunds: the transcript had
// the owner's "agree" and nothing of what was agreed). Once per card, and
// again only when its words change. The transcript only: never the public
// log, since a notice can carry the owner's limits.
func (t *test) noteRead(c component) {
	if c.Type != "notice" && c.Type != "confirm" {
		return
	}
	said := readWords(c)
	if t.noticesRead == nil {
		t.noticesRead = map[string]string{}
	}
	// Unchanged, or no words at all (the map's zero value): nothing to print.
	if t.noticesRead[c.ID] == said {
		return
	}
	t.noticesRead[c.ID] = said
	t.logf("the owner reads the %s %s: %s", c.Type, c.ID, said)
}

// readWords is a card's title and text on one line, whole, as the owner reads
// them: a notice's last line may be the limit that matters.
func readWords(c component) string {
	title, text := flat(c.Title), flat(c.Text)
	switch {
	case title != "" && text != "":
		return title + ": " + text
	case title != "":
		return title
	}
	return text
}
