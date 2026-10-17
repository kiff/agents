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
			"You never get the taf's key; tafs cloud has it claimed once the owner has confirmed.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"action":{"type":"string","enum":["submit"]},` +
			`"agent_id":{"type":"string","description":"the taf's KIFF agent id"},` +
			`"label":{"type":"string","description":"the taf's name, at most 100 characters"},` +
			`"cards":{"type":"array","items":{"type":"object"},"description":"the taf's Cards, on the gateway domain"}},` +
			`"required":["action","agent_id","cards"]}`),
	}
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
	body, _ := json.Marshal(map[string]any{"agent_id": in.AgentID, "label": in.Label, "cards": in.Cards})
	d, err := t.svc.Submit(ctx, t.tenant, t.key, body)
	var ke *kiffapi.Error
	if errors.As(err, &ke) {
		return tool.Result{Text: ke.Error(), IsError: true}, nil
	}
	if err != nil {
		return tool.Result{}, err
	}
	b, _ := json.Marshal(d)
	return tool.Result{Text: string(b)}, nil
}
