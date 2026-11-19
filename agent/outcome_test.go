package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/session"
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
