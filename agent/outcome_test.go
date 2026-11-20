package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/outcome"
)

// A request's outcome is the one request_outcome accepted, never the model's
// words: a value outside the run's list is refused and kept nowhere, the
// accepted one is the run's, and an answer that only says one keeps none.
func TestARequestsOutcomeIsTheOneItsToolAccepted(t *testing.T) {
	o := outcome.New([]string{"refunded", "refused", "needs_review"})
	m := &script{replies: []model.Reply{
		use("c1", outcome.Name, `{"value":"refunded_everything"}`),
		use("c2", outcome.Name, `{"value":"refused"}`),
		say("Outcome: refunded"),
	}}
	_, r := start(t, session.KindRun)
	out, _, err := agent.Run(context.Background(), newAgent(m, o), r, nil, "Check return A-1047.", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.RequestOutcome != "refused" {
		t.Errorf("the request's outcome: %q, want refused, the one the tool accepted", out.RequestOutcome)
	}
	if _, ok := out.Count()[outcome.Name]; ok {
		t.Errorf("request_outcome was counted with the calls that act: %+v", out.Count())
	}

	// Only words: no outcome.
	m = &script{replies: []model.Reply{say("Outcome: refunded")}}
	_, r = start(t, session.KindRun)
	out, _, err = agent.Run(context.Background(), newAgent(m, outcome.New([]string{"refunded"})), r, nil, "Check it.", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.RequestOutcome != "" {
		t.Errorf("words alone answered %q, want none", out.RequestOutcome)
	}
}

// The tool offers only the run's outcomes, and a run named none has no tool.
func TestTheOutcomeToolOffersOnlyTheRunsOutcomes(t *testing.T) {
	if outcome.New(nil) != nil {
		t.Fatal("a run with no outcomes got the tool")
	}
	o := outcome.New([]string{"refunded", "refused"})
	spec := o.Spec()
	if !strings.Contains(string(spec.InputSchema), `"enum":["refunded","refused"]`) {
		t.Errorf("schema %s, want the two outcomes as its enum", spec.InputSchema)
	}
	res, err := o.Call(context.Background(), []byte(`{"value":"approved"}`))
	if err != nil || !res.IsError || !strings.Contains(res.Text, "refunded, refused") {
		t.Errorf("an outcome not in the list: %+v, %v", res, err)
	}
	if res, _ := o.Call(context.Background(), []byte(`{"value":"refused"}`)); res.IsError {
		t.Errorf("an outcome in the list was refused: %+v", res)
	}
}

// The last outcome the tool accepted is the run's: a model may correct itself
// ("refunded", then "refused" once the refund was refused).
func TestTheLastAcceptedOutcomeWins(t *testing.T) {
	m := &script{replies: []model.Reply{
		use("c1", outcome.Name, `{"value":"refunded"}`),
		use("c2", outcome.Name, `{"value":"refused"}`),
		say("Done."),
	}}
	_, r := start(t, session.KindRun)
	out, _, err := agent.Run(context.Background(), newAgent(m, outcome.New([]string{"refunded", "refused"})), r, nil, "Check A-1047.", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.RequestOutcome != "refused" {
		t.Errorf("the request's outcome: %q, want refused, the later of two accepted", out.RequestOutcome)
	}
}

// erringOutcome is the outcome tool whose call errs whatever it's given: a
// call that errored answers nothing, even with a value in the list.
type erringOutcome struct{ *outcome.Tool }

func (e erringOutcome) Call(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{Text: "the call failed", IsError: true}, nil
}

// A request_outcome call that errored sets no outcome: only a call that went
// through answers the request.
func TestAnErroredOutcomeCallAnswersNothing(t *testing.T) {
	m := &script{replies: []model.Reply{use("c1", outcome.Name, `{"value":"refused"}`), say("Done.")}}
	_, r := start(t, session.KindRun)
	out, _, err := agent.Run(context.Background(), newAgent(m, erringOutcome{outcome.New([]string{"refunded", "refused"})}), r, nil, "Check A-1047.", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.RequestOutcome != "" {
		t.Errorf("an errored call answered %q, want none", out.RequestOutcome)
	}
}
