package kiff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/tool"
)

// YourStepName is the your_step tool's name, as the model sees it.
const YourStepName = "your_step"

// YourStep is the tool through which a taf hands its owner a step it may
// never take itself, such as approving a bank transfer or signing a
// contract: it prepares everything its Card allows, then names what is left,
// why it is the owner's, and the vendor's link (KIFF's POST
// /v1/me/handoffs, with the agent's own key).
//
// The step stays refused at the gateway, and the hand-off grants nothing. The
// run pauses on it as on a held call: KIFF's handoff_answered alert, or the
// hand-off's expiry, continues it, and the call is repeated with its
// operation ID to collect the owner's answer: "done", "skipped" or no answer.
// KIFF keeps the link as its host only.
type YourStep struct {
	api, key string
	hc       *http.Client
}

// NewYourStep returns the tool for an agent whose KIFF key is key, against
// KIFF's API at apiURL.
func NewYourStep(apiURL, key string, hc *http.Client) *YourStep {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &YourStep{api: strings.TrimSuffix(apiURL, "/"), key: key, hc: hc}
}

var yourStepSchema = json.RawMessage(`{"type":"object","properties":{` +
	`"what":{"type":"string","description":"What the owner does, in their language, in one line: \"Approve 3 supplier payments, €4,210 in all\"."},` +
	`"why":{"type":"string","description":"Why it is theirs to do, in one line: \"Your bank asks you to approve transfers yourself\"."},` +
	`"link":{"type":"string","description":"The vendor's https link where the owner does it, as the vendor returned it. Optional."},` +
	`"about":{"type":"string","description":"The case, as your calls' kiff_about name it (\"Batch 11 Nov\"). Set it: with it, a later run that finds the same step waiting doesn't hand it over twice."}},` +
	`"required":["what","why"],"additionalProperties":false}`)

// Spec says when to use it: only for a step the agent may never take, once
// everything it may do is done.
func (t *YourStep) Spec() model.ToolSpec {
	return model.ToolSpec{Name: YourStepName, InputSchema: yourStepSchema,
		Description: "Hand your owner a step you may never take yourself (approving a bank transfer, signing, submitting a form only they may), " +
			"once you've prepared everything your Card allows. The owner sees \"Your step\" with what, why and the link, and answers \"Done\" or \"Skip it\". " +
			"Your run waits for the answer. Never use it for a call KIFF held or refused: those have their own answers. " +
			"After \"Done\", read the vendor to see it was done if you can; after \"Skip it\", withdraw what you prepared where you can."}
}

// Access: it changes nothing outside the agent; KIFF records the hand-off.
func (*YourStep) Access() tool.Access { return tool.Own }

// Gateway: it goes to KIFF's API, not through the gateway.
func (*YourStep) Gateway() bool { return false }

// handoff is KIFF's view of a hand-off.
type handoff struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	LinkHost  string    `json:"link_host"`
	ExpiresAt time.Time `json:"expires_at"`
	// AlreadyWaiting: an earlier call (a run before this one, which found
	// the same work) handed this step over, and it still waits.
	AlreadyWaiting bool `json:"already_waiting"`
}

// Call raises the hand-off with the call's operation ID: the same each time
// this call is made, so a crash or a repeat raises it once.
func (t *YourStep) Call(ctx context.Context, input json.RawMessage) (tool.Result, error) {
	op := ""
	if ref, ok := run.RefFrom(ctx); ok && ref.Run != "" {
		if id := tool.CallFrom(ctx); id != "" {
			op = run.OperationOf(ref.Run, id, input)
		}
	}
	if op == "" {
		return tool.Result{Text: "your_step works only inside a run.", IsError: true}, nil
	}
	if _, trial := run.TrialFrom(ctx); trial {
		return tool.Result{Text: "This is a trial: the step would be handed to your owner as \"Your step\". Nothing was handed over.", IsError: false}, nil
	}
	res, _, err := t.raise(ctx, input, op)
	return res, err
}

// Collect is the hand-off's outcome for a run that paused on it, once KIFF
// said it closed: the same call again, with its operation ID. One still
// waiting is ErrNotFinal, and the run stays paused.
func (t *YourStep) Collect(ctx context.Context, q run.Requirement) (tool.Result, error) {
	if q.Operation == "" {
		return tool.Result{}, fmt.Errorf("kiff: your_step call %s has no operation ID to collect it by", q.CallID)
	}
	res, h, err := t.raise(ctx, q.Input, q.Operation)
	if err != nil {
		return tool.Result{}, err
	}
	if res.Held() {
		return tool.Result{}, fmt.Errorf("%w: your_step call %s still waits for the owner", ErrNotFinal, q.CallID)
	}
	if h.Status == "done" {
		// Answered done: the run's first read of the vendor about the same case is the step's check.
		id := h.ID
		run.ChecksFrom(ctx).Await(run.CheckWait{Handoff: id, About: stepAbout(q.Input), Post: func(ctx context.Context, op string) error {
			return t.check(ctx, id, op)
		}})
	}
	return res, nil
}

// stepAbout is a your_step call's about, or "".
func stepAbout(input json.RawMessage) string {
	var in struct {
		About string `json:"about"`
	}
	_ = json.Unmarshal(input, &in)
	return in.About
}

// check names the read that checked a step answered done (KIFF's POST /v1/me/handoffs/{id}/check), by the read's
// operation ID. KIFF takes it only for this agent's sent call made after the answer, once.
func (t *YourStep) check(ctx context.Context, handoff, op string) error {
	body, _ := json.Marshal(map[string]string{"operation": op})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.api+"/v1/me/handoffs/"+url.PathEscape(handoff)+"/check", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("kiff: your_step check: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+t.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.hc.Do(req)
	if err != nil {
		return fmt.Errorf("kiff: your_step check: %w", scrub(err, t.key))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kiff: your_step check: KIFF answered %d", resp.StatusCode)
	}
	return nil
}

func (t *YourStep) raise(ctx context.Context, input json.RawMessage, op string) (tool.Result, handoff, error) {
	var in struct {
		What  string `json:"what"`
		Why   string `json:"why"`
		Link  string `json:"link"`
		About string `json:"about"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return tool.Result{Text: "your_step takes {what, why, link?, about?}.", IsError: true}, handoff{}, nil
	}
	prepared := preparedBy(ctx, in.About)
	raw, status, err := t.post(ctx, in.What, in.Why, in.Link, in.About, op, prepared)
	if err == nil && status == http.StatusUnprocessableEntity && len(prepared) > 0 && refusalCode(raw) == "prepared_by_invalid" {
		// KIFF wouldn't take one of the calls as this step's preparation: the step reaching its owner matters more
		// than the link in its receipt, so it's raised again without them.
		raw, status, err = t.post(ctx, in.What, in.Why, in.Link, in.About, op, nil)
	}
	if err != nil {
		return tool.Result{}, handoff{}, err
	}
	return t.answer(raw, status, op)
}

// post sends the raise to KIFF and returns its answer's body and status.
func (t *YourStep) post(ctx context.Context, what, why, link, about, op string, prepared []string) ([]byte, int, error) {
	payload := map[string]any{"what": what, "why": why, "link": link, "about": about, "operation": op}
	if len(prepared) > 0 {
		payload["prepared_by"] = prepared
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.api+"/v1/me/handoffs", bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("kiff: your_step: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+t.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("kiff: your_step: %w", scrub(err, t.key))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, 0, fmt.Errorf("kiff: your_step: %w", err)
	}
	return raw, resp.StatusCode, nil
}

// refusalCode is the error code of KIFF's refusal, or "".
func refusalCode(raw []byte) string {
	var e struct{ Error string }
	_ = json.Unmarshal(raw, &e)
	return e.Error
}

// answer is KIFF's answer to a raise, as the run reads it.
func (t *YourStep) answer(raw []byte, status int, op string) (tool.Result, handoff, error) {
	switch status {
	case http.StatusOK, http.StatusCreated:
	case http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusTooManyRequests, http.StatusForbidden:
		// KIFF's refusal, for the model to act on: nothing was handed over.
		var e struct{ Error, Detail string }
		_ = json.Unmarshal(raw, &e)
		why := strings.TrimSpace(e.Detail)
		if why == "" {
			why = e.Error
		}
		return tool.Result{Text: "Nothing was handed to your owner. KIFF said: " + why, IsError: true}, handoff{}, nil
	default:
		return tool.Result{}, handoff{}, fmt.Errorf("kiff: your_step: KIFF answered %d", status)
	}
	var h handoff
	if err := json.Unmarshal(raw, &h); err != nil || h.ID == "" {
		return tool.Result{}, handoff{}, errors.New("kiff: your_step: KIFF's answer names no hand-off")
	}
	return yourStepResult(h, op), h, nil
}

// maxPreparedBy is the most calls a hand-off names as its preparation (KIFF's MaxPreparedBy).
const maxPreparedBy = 20

// preparedBy is what the run did to prepare the step, as KIFF names calls: the operation IDs of its calls that
// acted, went through and that KIFF recorded (each with KIFF's invocation), newest first, at most maxPreparedBy.
// With about, only the calls about the same case (their kiff_about). A call kept without its input is left out:
// its operation ID can't be told from its call ID alone.
func preparedBy(ctx context.Context, about string) []string {
	ref, ok := run.RefFrom(ctx)
	if !ok || ref.Run == "" {
		return nil
	}
	done := run.DoneFrom(ctx)
	var out []string
	seen := map[string]bool{}
	for i := len(done) - 1; i >= 0 && len(out) < maxPreparedBy; i-- {
		d := done[i]
		if d.Invocation == "" || len(d.Input) == 0 {
			continue
		}
		if about != "" && aboutOf(d.Input) != about {
			continue
		}
		op := run.OperationOf(ref.Run, d.CallID, d.Input)
		if !seen[op] {
			seen[op] = true
			out = append(out, op)
		}
	}
	return out
}

// aboutOf is a call's kiff_about, or "".
func aboutOf(input json.RawMessage) string {
	var args struct {
		About string `json:"kiff_about"`
	}
	_ = json.Unmarshal(input, &args)
	return args.About
}

// yourStepResult is the hand-off as the run reads it: waiting pauses the run
// as a held call does; an answer is the call's result.
func yourStepResult(h handoff, op string) tool.Result {
	switch h.Status {
	case "waiting":
		if h.AlreadyWaiting {
			// Another run handed it over and waits on it: this run doesn't.
			return tool.Result{Text: "This step is already with your owner as \"Your step\", handed over by an earlier run. Don't hand it over again: leave it to them, and say so."}
		}
		return tool.Result{State: tool.StateHeld, Hold: &tool.Hold{Operation: op, Exception: h.ID, Expires: h.ExpiresAt},
			Text: "Handed to your owner as \"Your step\". Your run waits for their answer."}
	case "done":
		return tool.Result{Text: "Your owner says they did it. If you can read the vendor, check it was done before you close the case, and say what you saw."}
	case "skipped":
		return tool.Result{Text: "Your owner chose not to do this step. Withdraw what you prepared for it where you can, and say what is left."}
	case "no_answer":
		return tool.Result{Text: "Your owner didn't answer in time. The step is not done: leave the case open, and say so.", IsError: true}
	}
	return tool.Result{Text: "KIFF says the step is " + h.Status + ".", IsError: true}
}
