package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// weeklyDecision is tafs cloud's answer to an approval tap or its report.
type weeklyDecision struct {
	Outcome string     `json:"outcome"`
	Answer  string     `json:"answer"`
	Note    string     `json:"note"`
	Ask     *weeklyAsk `json:"ask"`
	Session string     `json:"session"`
	Run     string     `json:"run"`
}

// weeklyCloud is tafs cloud's component route, as the owner's phone calls it.
type weeklyCloud interface {
	// Approval is Approve tapped on a card (POST …/components/{c}/approval),
	// with the phone's signed read of its grants.
	Approval(ctx context.Context, component string, body map[string]any) (weeklyDecision, error)
	// Report is what came of the asked call (POST …/approval/{ask}), signed
	// as a setup call to KIFF.
	Report(ctx context.Context, component, ask string, body map[string]any) (weeklyDecision, error)
}

// weeklyPhone is what the phone reads and does outside tafs cloud: planque's
// grant as it shows it, KIFF's draft, and the calls an ask names.
type weeklyPhone interface {
	// Grant is the grant's digest as the phone reads it, and its tools.
	Grant(ctx context.Context, grant string) (digest string, tools []weeklyTool, err error)
	DraftVersion(ctx context.Context, draft string) (string, error)
	ConfirmDraft(ctx context.Context, draft, version string) error
	// ApproveConnect sends planque's approve body as tafs cloud built it, and
	// returns the grant's status after.
	ApproveConnect(ctx context.Context, grant string, body json.RawMessage) (string, error)
}

// weeklyShown is an approval card as the thread shows it.
type weeklyShown struct {
	ID, Session  string
	Grant, Draft string
	Tools        []string
	SignsCard    *bool
}

// weeklyTool is one tool of a grant: its name without the grant's suffix,
// and the vendor's operation it calls.
type weeklyTool struct{ Name, Op string }

// weeklyJob says whether a grant's tools are the job's (kiff-qa (c)).
type weeklyJob func(tools []weeklyTool) bool

// exactly is a catalog entry's job: its tools are exactly these, by the
// catalog's names (the key family).
func exactly(names ...string) weeklyJob {
	return func(tools []weeklyTool) bool { return sameTools(toolNames(tools), names, names) }
}

// within is a raw request's job (the own-key family, kiff-qa 08:47): each
// tool calls an operation the vendor's own description has, and there's at
// least one.
func within(ops ...string) weeklyJob {
	return func(tools []weeklyTool) bool {
		for _, t := range tools {
			if !slices.Contains(ops, t.Op) {
				return false
			}
		}
		return len(tools) > 0
	}
}

func toolNames(tools []weeklyTool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	return out
}

// errMoreThanTheJob is kiff-qa's (c): the phone refuses a connection that
// asks for more than the card showed and the job needs.
var errMoreThanTheJob = errors.New("asked for more than the job")

// approveCard is one tap on an approval card, through tafs cloud's component
// route only: the ask, the phone's own check of what it would sign, the call
// the ask names, and the signed report. It returns tafs cloud's answer to
// Coach. Anything the phone can't rebuild from what the card showed is
// refused, never signed.
func approveCard(ctx context.Context, cloud weeklyCloud, phone weeklyPhone, c weeklyShown, tenant string, job weeklyJob) (string, error) {
	card := weeklyCard{Grant: c.Grant, Draft: c.Draft, SignsCard: c.SignsCard}
	body := map[string]any{"grant": c.Grant, "session": c.Session}
	if c.Grant != "" {
		digest, tools, err := phone.Grant(ctx, c.Grant)
		if err != nil {
			return "", fmt.Errorf("the phone's read of the grant: %w", err)
		}
		// The card shows exactly the grant's tools, and they're the job's.
		names := toolNames(tools)
		shown := c.Tools
		if len(shown) == 0 {
			shown = names
		}
		if !sameTools(names, shown, names) || !job(tools) {
			return "", errMoreThanTheJob
		}
		card.ShownDigest, card.Tools = digest, names
		body["shown_digest"] = digest
	}
	if c.Draft != "" {
		v, err := phone.DraftVersion(ctx, c.Draft)
		if err != nil {
			return "", fmt.Errorf("the phone's read of the draft: %w", err)
		}
		card.ShownDraft = v
		body["draft"], body["shown_draft"] = c.Draft, v
	}
	d, err := cloud.Approval(ctx, c.ID, body)
	if err != nil {
		return "", fmt.Errorf("the approval route: %w", err)
	}
	switch d.Outcome {
	case "answered":
		return answered(d)
	case "read_again":
		return "", fmt.Errorf("tafs cloud says the card is out of date: %s", d.Note)
	case "sign":
	default:
		return "", fmt.Errorf("tafs cloud answered %q", d.Outcome)
	}
	if d.Ask == nil {
		return "", errors.New("tafs cloud asked for a signature and named no ask")
	}
	ask := *d.Ask
	if want := expectedApprovalMessage(ask, card, tenant); want == "" || ask.Message != want {
		return "", errors.New("the ask isn't this card's: the phone signs nothing it can't rebuild")
	}
	if !stepMatchesMark(card.SignsCard, ask.Step) {
		return "", errors.New("the ask's step isn't what the card drew")
	}
	report := map[string]any{"session": c.Session}
	switch ask.Step {
	case "cards":
		if err := phone.ConfirmDraft(ctx, ask.KIFF.Draft, ask.KIFF.Version); err != nil {
			report["result"], report["why"] = "failed", scrubWhy(err.Error())
		} else {
			report["result"] = "confirmed"
		}
	case "connect":
		status, err := phone.ApproveConnect(ctx, ask.Planque.Grant, ask.Planque.Body)
		switch {
		case err != nil:
			report["result"], report["why"] = "failed", scrubWhy(err.Error())
		case status == "connected" || status == "listed":
			report["result"] = status
		default:
			report["result"], report["status"] = "not_connected", status
		}
	}
	r, err := cloud.Report(ctx, c.ID, ask.ID, report)
	if err != nil {
		return "", fmt.Errorf("the signed report: %w", err)
	}
	return answered(r)
}

// answered is tafs cloud's answer to Coach, or an error for one it answered
// as failed.
func answered(d weeklyDecision) (string, error) {
	if d.Outcome != "answered" {
		return "", fmt.Errorf("tafs cloud answered the report %q", d.Outcome)
	}
	if strings.HasPrefix(d.Answer, "failed") {
		return d.Answer, fmt.Errorf("tafs cloud answered Coach %q", d.Answer)
	}
	return d.Answer, nil
}
