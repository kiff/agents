// Package drafts is the tool through which the setup agent submits a taf's
// KIFF draft: the taf's agent and its Cards (kiff/agents#91). A draft grants
// nothing until the owner's phone confirms it, so this is an Own tool. KIFF
// answers with the draft's claim secret, which mints the taf's key: the tool
// keeps it sealed and the model never sees it (drafts.Service).
package drafts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/kiff/agents/drafts"
	"github.com/kiff/agents/drafts/kiffapi"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/tool"
)

// Name is the tool's name, as the model sees it.
const Name = "kiff_draft"

// Tool submits drafts for one agent of one tenant.
type Tool struct {
	svc         *drafts.Service
	tenant, key string
}

// New returns the draft tool for the agent whose KIFF key is key.
func New(svc *drafts.Service, tenant, key string) *Tool {
	return &Tool{svc: svc, tenant: tenant, key: key}
}

func (t *Tool) Access() tool.Access { return tool.Own }
func (t *Tool) Gateway() bool       { return false }

func (t *Tool) Spec() model.ToolSpec {
	return model.ToolSpec{
		Name: Name,
		Description: "Submit a taf's KIFF draft: its agent and its Cards, as KIFF takes them. It grants nothing until the owner confirms it on their phone: " +
			"show an approval naming the draft (and the taf's planque grant). submit {agent_id, label, cards}: returns the draft's id, agent and status. " +
			"A Card named by an id the agent holds alone revises that Card. An id KIFF refuses as a Card the agent doesn't hold alone and in force goes again under the agent's name (\"reads\" as \"<agent_id>-reads\"), a new Card, and the result says so. " +
			"You never get the taf's key; tafs cloud has it claimed once the owner has confirmed.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"action":{"type":"string","enum":["submit"]},` +
			`"agent_id":{"type":"string","description":"the taf's KIFF agent id"},` +
			`"label":{"type":"string","description":"the taf's name, at most 100 characters"},` +
			`"cards":{"type":"array","items":{"type":"object"},"description":"the taf's Cards, on the gateway domain, each as KIFF issues it: ` +
			`{id, subject (the agent id), domain: \"gateway\", grants: [{action (a connected tool's name), per_action_max?: [{quantity: \"sum(<numeric argument>)\", max}]}], ` +
			`aggregates (at least one): [{quantity: \"count\" or \"sum(<numeric argument>)\", limit, window: \"calendar_day\", \"rolling_1h\", \"rolling_24h\", \"rolling_7d\" (any 7 days) or \"lifetime\", ` +
			`per?: a string argument the tools declare, to count each of its values apart (\"1 reminder a week for each invoice\": quantity \"count\", limit 1, window \"rolling_7d\", per \"invoice_id\"; ` +
			`it counts only the tools that declare that argument, holds such a call that names none, and is beside the Card's other totals, never instead of one)}], ` +
			`on_exceed?: \"owner\" (held for the owner, the default) or \"refuse\"}. ` +
			`max and limit are in the argument's own units, as the call sends it: with the tool's amount_scale 2, 50.00 is 5000; with scale 0, 50. ` +
			`Convert what the owner said into those units, and say it back in theirs."}},` +
			`"required":["action","agent_id","cards"]}`),
	}
}

// MaxStated is how many of a session's drafts State looks up: the latest.
const MaxStated = 4

// State is where each draft the session's components name stands now, from
// what this runtime keeps of it: waiting for the owner's confirmation, or
// confirmed, its key claimed for the taf. A draft it keeps nothing of (a
// revision, or another runtime's) is said to be unknown here.
func (t *Tool) State(ctx context.Context, shown []json.RawMessage) (string, error) {
	ids := tool.Named(shown, "draft", MaxStated)
	if len(ids) == 0 {
		return "", nil
	}
	// Each draft is read at once, keeping their order: a run's start waits
	// for the slowest read, not their sum.
	lines := make([]string, len(ids))
	tool.Each(len(ids), MaxStated, func(i int) {
		id := ids[i]
		rec, err := t.svc.Store.Get(ctx, t.tenant, id)
		switch {
		case errors.Is(err, drafts.ErrNotFound):
			lines[i] = fmt.Sprintf("- %s: nothing kept of it here (a revision of a taf's Cards, or not submitted here)\n", id)
		case err != nil:
			lines[i] = fmt.Sprintf("- %s: couldn't be read now\n", id)
		case rec.Claimed:
			lines[i] = fmt.Sprintf("- %s, for agent %s: confirmed by the owner, and the taf has its key\n", id, rec.AgentID)
		case rec.Mismatch:
			lines[i] = fmt.Sprintf("- %s, for agent %s: confirmed, but KIFF's key wasn't this agent's, so it wasn't kept\n", id, rec.AgentID)
		default:
			lines[i] = fmt.Sprintf("- %s, for agent %s: submitted; not claimed yet (the owner hasn't confirmed it, or tafs hasn't taken its key)\n", id, rec.AgentID)
		}
	})
	var b strings.Builder
	b.WriteString("Drafts in this conversation, as kept now:\n")
	for _, l := range lines {
		b.WriteString(l)
	}
	return b.String(), nil
}

// takenID is KIFF's refusal of a draft's Card whose id is a Card the agent
// doesn't hold alone and in force (kiff-cloud's draftCards), naming it by
// its place in the draft.
var takenID = regexp.MustCompile(`^card (\d+): a Card with this id exists, and it isn't this agent's`)

// refusedCard is the place in the draft of the Card KIFF refused for its id
// (takenID), or -1 for any other answer.
func refusedCard(err error) int {
	var ke *kiffapi.Error
	if !errors.As(err, &ke) || ke.Status != http.StatusConflict {
		return -1
	}
	m := takenID.FindStringSubmatch(ke.Message)
	if m == nil {
		return -1
	}
	i, err := strconv.Atoi(m[1])
	if err != nil {
		return -1
	}
	return i
}

// underAgent puts card's id under agent's own name ("refunds-s7zax3qf-reads"
// for "reads") and says what it was, unless it is there already: then there
// is nothing to rename, and KIFF's refusal goes to the model.
func underAgent(agent string, card map[string]json.RawMessage) (string, bool) {
	var id string
	if json.Unmarshal(card["id"], &id) != nil || id == "" || id == agent || strings.HasPrefix(id, agent+"-") {
		return "", false
	}
	card["id"], _ = json.Marshal(agent + "-" + id)
	return id, true
}

// Call submits the draft. What KIFF refuses comes back as a result the
// model can read and correct.
func (t *Tool) Call(ctx context.Context, input json.RawMessage) (tool.Result, error) {
	var in struct {
		Action  string          `json:"action"`
		AgentID string          `json:"agent_id"`
		Label   string          `json:"label"`
		Cards   json.RawMessage `json:"cards"`
	}
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || in.Action != "submit" {
		return tool.Result{Text: "the input must be {action: submit, agent_id, label, cards}", IsError: true}, nil
	}
	if in.AgentID == "" || len(in.Cards) == 0 || in.Cards[0] != '[' {
		return tool.Result{Text: "submit needs agent_id and cards (an array)", IsError: true}, nil
	}
	var cards []map[string]json.RawMessage
	if err := json.Unmarshal(in.Cards, &cards); err != nil {
		return tool.Result{Text: "cards is an array of Cards, each an object with its id", IsError: true}, nil
	}
	// Each id goes as the model named it, so a Card the agent holds is
	// revised. An account keeps the Cards of tafs made before (an Eval's
	// earlier runs), and KIFF refuses a draft naming one (409), which cost the
	// setup a turn each time (Eval 42206117004): the Card KIFF names goes
	// again under the agent's own name, once each, never a Card it accepted.
	var renamed []string
	for {
		body, _ := json.Marshal(map[string]any{"agent_id": in.AgentID, "label": in.Label, "cards": cards})
		d, err := t.svc.Submit(ctx, t.tenant, t.key, body)
		if i := refusedCard(err); i >= 0 && i < len(cards) {
			if was, ok := underAgent(in.AgentID, cards[i]); ok {
				renamed = append(renamed, strconv.Quote(was))
				continue
			}
		}
		var ke *kiffapi.Error
		if errors.As(err, &ke) {
			return tool.Result{Text: ke.Error(), IsError: true}, nil
		}
		if err != nil {
			return tool.Result{}, err
		}
		b, _ := json.Marshal(d)
		text := string(b)
		if len(renamed) > 0 {
			text += fmt.Sprintf("\nKIFF has a Card with the id %s that this agent doesn't hold alone and in force, so the draft names it under the agent's name (%s-…): "+
				"a new Card, not a revision of that one.", strings.Join(renamed, ", "), in.AgentID)
		}
		return tool.Result{Text: text}, nil
	}
}
