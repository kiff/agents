// Package followup is the tool through which a taf sets its own one-off
// follow-up: "check this refund again tomorrow at 9". The
// schedule service keeps the bounds (once, within seven days, at most three
// waiting, never from a follow-up's own run); the model's words never do.
// It changes only the agent's own schedules, so it doesn't go through
// KIFF's gateway, and each follow-up's run works inside the same Card.
package followup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/schedule"
	"github.com/kiff/agents/tool"
)

// Name is the tool's name, as the model sees it.
const Name = "follow_up"

// prefix starts every follow-up's schedule ID, so its agent's other
// schedules are never listed or removed through this tool.
const prefix = "fu-"

// Tool is one agent's follow-ups.
type Tool struct {
	sv            *schedule.Service
	runs          run.Runs
	tenant, agent string
	version       int
}

// New returns the follow-up tool for one agent at version: a follow-up it
// sets fires only under that version.
func New(sv *schedule.Service, runs run.Runs, tenant, agent string, version int) *Tool {
	return &Tool{sv: sv, runs: runs, tenant: tenant, agent: agent, version: version}
}

func (t *Tool) Access() tool.Access { return tool.Own }
func (t *Tool) Gateway() bool       { return false }

func (t *Tool) Spec() model.ToolSpec {
	return model.ToolSpec{
		Name: Name,
		Description: fmt.Sprintf("Your one-off follow-ups: a run of you, once, at a time you set, to check on something again. "+
			"set: at a time (RFC 3339 with its offset, such as 2026-11-03T09:00:00+01:00), in the next %d days, with what to check; "+
			"list: the ones waiting; remove: one by its id. At most %d wait at once, and a run a follow-up started can't set another. "+
			"A follow-up works inside your Card, like any run.", int(schedule.FollowUpWithin.Hours()/24), schedule.MaxFollowUps),
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"action":{"type":"string","enum":["set","list","remove"]},` +
			`"at":{"type":"string","description":"when it runs, RFC 3339 with its offset (set only)"},` +
			`"check":{"type":"string","description":"what the follow-up run checks, in your own words (set only)"},` +
			`"id":{"type":"string","description":"the follow-up's id (remove only)"}},` +
			`"required":["action"]}`),
	}
}

// Call sets, lists or removes a follow-up. A request outside the bounds is a
// Result with IsError, saying which, so the model can tell the owner.
func (t *Tool) Call(ctx context.Context, input json.RawMessage) (tool.Result, error) {
	var in struct {
		Action string `json:"action"`
		At     string `json:"at"`
		Check  string `json:"check"`
		ID     string `json:"id"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return tool.Result{Text: "the input must be {action, at, check, id}", IsError: true}, nil
	}
	if _, trial := run.TrialFrom(ctx); trial && in.Action != "list" {
		return tool.Result{Text: "This is a trial: follow-ups are listed, not set or removed."}, nil
	}
	switch in.Action {
	case "list":
		return t.list(ctx)
	case "remove":
		s, err := t.sv.Store.Get(ctx, t.tenant, in.ID)
		if errors.Is(err, schedule.ErrNotFound) || err == nil && (!s.FollowUp || s.Agent != t.agent) {
			return tool.Result{Text: "No follow-up of yours has that id.", IsError: true}, nil
		}
		if err != nil {
			return tool.Result{}, err
		}
		if err := t.sv.Delete(ctx, t.tenant, in.ID); err != nil {
			return tool.Result{}, err
		}
		return tool.Result{Text: "Removed " + in.ID + "."}, nil
	case "set":
	default:
		return tool.Result{Text: "action is set, list or remove", IsError: true}, nil
	}
	at, err := time.Parse(time.RFC3339, in.At)
	check := strings.TrimSpace(in.Check)
	if err != nil || check == "" || len(check) > schedule.MaxInput {
		return tool.Result{Text: fmt.Sprintf("set needs at, RFC 3339 with its offset, and check, 1 to %d bytes", schedule.MaxInput), IsError: true}, nil
	}
	chained, err := t.fromFollowUp(ctx)
	if err != nil {
		return tool.Result{}, err
	}
	s := schedule.Schedule{Tenant: t.tenant, ID: newID(), Agent: t.agent,
		At: at.Truncate(time.Minute), Input: check, Version: t.version}
	switch err := t.sv.FollowUp(ctx, s, chained); {
	case errors.Is(err, schedule.ErrFollowUpChain), errors.Is(err, schedule.ErrFollowUpLimit), errors.Is(err, schedule.ErrFollowUpRange):
		return tool.Result{Text: strings.TrimPrefix(err.Error(), "schedule: "), IsError: true}, nil
	case err != nil:
		return tool.Result{}, err
	}
	return tool.Result{Text: fmt.Sprintf("Set %s, once, at %s.", s.ID, s.At.Format(time.RFC3339))}, nil
}

// fromFollowUp reports whether the calling run was started by a follow-up.
func (t *Tool) fromFollowUp(ctx context.Context) (bool, error) {
	ref, ok := run.RefFrom(ctx)
	if !ok {
		return false, nil
	}
	r, err := t.runs.Get(ctx, t.tenant, ref.Run)
	if errors.Is(err, run.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if r.Trigger != run.BySchedule {
		return false, nil
	}
	if strings.HasPrefix(r.Source, prefix) {
		return true, nil
	}
	s, err := t.sv.Store.Get(ctx, t.tenant, r.Source)
	if errors.Is(err, schedule.ErrNotFound) {
		return false, nil
	}
	return err == nil && s.FollowUp, err
}

func (t *Tool) list(ctx context.Context) (tool.Result, error) {
	all, err := t.sv.Store.List(ctx, t.tenant)
	if err != nil {
		return tool.Result{}, err
	}
	var b strings.Builder
	for _, s := range all {
		if s.FollowUp && s.Enabled && s.Agent == t.agent {
			fmt.Fprintf(&b, "%s at %s: %s\n", s.ID, s.At.Format(time.RFC3339), s.Input)
		}
	}
	if b.Len() == 0 {
		return tool.Result{Text: "No follow-ups waiting."}, nil
	}
	return tool.Result{Text: strings.TrimSpace(b.String())}, nil
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}
