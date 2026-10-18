package main

import (
	"context"
	"strings"
	"testing"

	"github.com/kiff/agents/eval"
	"github.com/kiff/agents/model"
)

// verdicts is a judge model that passes or fails each rule by a word in it,
// and keeps what it was asked.
type verdicts struct {
	fail  []string // a rule containing one of these fails
	asked []string
}

func (v *verdicts) Turn(_ context.Context, req model.Request, _ func(string)) (model.Reply, error) {
	prompt := req.Messages[0].Blocks[0].Text
	v.asked = append(v.asked, prompt)
	pass := "true"
	for _, f := range v.fail {
		if strings.Contains(prompt, f) {
			pass = "false"
		}
	}
	return model.Reply{Blocks: []model.Block{{Kind: model.Text, Text: `{"pass": ` + pass + `, "why": "judged"}`}}}, nil
}

func TestTheSetupChatIsGraded(t *testing.T) {
	var s said
	s.messages = []string{"¿Con qué proveedor cobras?", strings.Repeat("palabra ", maxWords+1)}
	c := component{Type: "confirm", ID: "modo", Title: "Modo de Stripe", Text: "¿Prueba o real?", Note: "Puedes cambiarlo luego."}
	c.Options = append(c.Options, struct {
		Value string `json:"value"`
		Label string `json:"label"`
	}{"test", "Prueba"})
	s.card(c)
	s.card(c) // a card shown again is one card

	v := &verdicts{fail: []string{"asks the owner to give"}}
	checks, err := gradeSetup(context.Background(), &eval.Judge{Model: v, ModelID: "judge", Price: model.Price{Input: 1, Output: 1}},
		owners["es"], s, "taf-reembolsos-x7k3")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range checks {
		got[c.Name] = c.Pass
	}
	want := map[string]bool{"in the owner's language": true, "brief": false, "asks for no id": false,
		"names the agent from the taf's name": true, "offers choices to tap": true}
	for name, pass := range want {
		if p, ok := got[name]; !ok || p != pass {
			t.Errorf("%s: pass %v (present %v), want %v", name, p, ok, pass)
		}
	}
	if len(s.order) != 1 {
		t.Fatalf("cards %v", s.order)
	}
	// The language check reads Setup's cards too: their title, text and labels.
	if !strings.Contains(v.asked[0], "Spanish") || !strings.Contains(v.asked[0], "Modo de Stripe") || !strings.Contains(v.asked[0], "Prueba") ||
		!strings.Contains(v.asked[0], "Puedes cambiarlo luego.") {
		t.Fatalf("the language check read %q", v.asked[0])
	}
	// So does the id check: a confirm can ask for an id too.
	if !strings.Contains(v.asked[1], "asks the owner to give") || !strings.Contains(v.asked[1], "Modo de Stripe") {
		t.Fatalf("the id check read %q", v.asked[1])
	}
	if !counted["brief"] || counted["asks for no id"] {
		t.Fatal("only the checks graded without the judge print their reasons in CI")
	}

	// An agent id that doesn't carry the taf's name fails the naming check.
	checks, _ = gradeSetup(context.Background(), &eval.Judge{Model: &verdicts{}, ModelID: "judge"}, owners["es"], s, "taf-x7k3")
	for _, c := range checks {
		if c.Name == "names the agent from the taf's name" && c.Pass {
			t.Fatal("an agent id without the taf's name passed")
		}
	}
	if slug("Reembolsos de Tienda") != "reembolsos-de-tienda" {
		t.Fatalf("slug %q", slug("Reembolsos de Tienda"))
	}
}
