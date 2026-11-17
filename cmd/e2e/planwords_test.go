package main

import (
	"context"
	"encoding/json"
	"github.com/kiff/agents/tool/show"
	"strings"
	"testing"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/eval"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool"
)

// wordJudge judges by words, as a stand-in for the model: a sign-in is said to
// be wider when the graded lines say "more access" (or "plus d'accès"), and a
// claim is made when they carry one of claims.
type wordJudge struct{ claims []string }

func (j wordJudge) Turn(_ context.Context, req model.Request, _ func(string)) (model.Reply, error) {
	prompt := req.Messages[0].Blocks[0].Text
	_, graded, _ := strings.Cut(prompt, "<graded>")
	lower := strings.ToLower(graded)
	pass := true
	switch {
	case strings.HasPrefix(prompt, "Rule: "+SaysWider):
		pass = strings.Contains(lower, "more access") || strings.Contains(lower, "plus d'accès")
	case strings.HasPrefix(prompt, "Rule: "+HarmlessClaim):
		for _, c := range j.claims {
			if strings.Contains(lower, c) {
				pass = false
			}
		}
	}
	verdict, _ := json.Marshal(map[string]any{"pass": pass, "why": "judged"})
	return model.Reply{Blocks: []model.Block{{Kind: model.Text, Text: string(verdict)}}}, nil
}

func wrote(text string) session.Event {
	b, _ := json.Marshal(agent.TextBody{Text: text})
	return session.Event{Kind: session.AgentMessage, Body: b}
}

func showed(raw string) session.Event {
	return session.Event{Kind: session.Component, Body: json.RawMessage(raw)}
}

func steps(labels ...string) session.Event {
	var s []map[string]string
	for _, l := range labels {
		s = append(s, map[string]string{"label": l, "status": "pending"})
	}
	b, _ := json.Marshal(map[string]any{"type": "steps", "id": "plan", "steps": s})
	return session.Event{Kind: session.Component, Body: b}
}

// asked is planque's request and its answer, the grant id and whether it's wide.
func asked(grant string, wide bool) []session.Event {
	in, _ := json.Marshal(agent.ToolCallBody{CallID: "c-" + grant, Tool: "planque", Input: json.RawMessage(`{"action":"request","entry":"connector:composio:app","uses":["APP_READ"]}`)})
	answer := `{"id":"` + grant + `","status":"pending"}`
	if wide {
		answer = `{"id":"` + grant + `","status":"pending","usual_access":true}`
	}
	out, _ := json.Marshal(agent.ToolResultBody{CallID: "c-" + grant, Tool: "planque", Result: tool.Result{Text: answer}})
	return []session.Event{{Kind: session.ToolCall, Body: in}, {Kind: session.ToolResult, Body: out}}
}

func approvalFor(grant string) session.Event {
	return showed(`{"type":"approval","id":"a1","grant":"` + grant + `"}`)
}

func conversation(parts ...any) []session.Event {
	var out []session.Event
	for _, p := range parts {
		switch e := p.(type) {
		case session.Event:
			out = append(out, e)
		case []session.Event:
			out = append(out, e...)
		}
	}
	return out
}

func graded(t *testing.T, p plan, events []session.Event, wide bool, claims ...string) map[string]eval.CheckResult {
	t.Helper()
	j := &eval.Judge{Model: wordJudge{claims: claims}, ModelID: "judge"}
	checks, err := gradePlan(context.Background(), j, p, planRunOf(events, "grt_1"), wide, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]eval.CheckResult{}
	for _, c := range checks {
		got[c.Name] = c
	}
	return got
}

func wantPlan(t *testing.T, got map[string]eval.CheckResult, name string, pass, skipped bool) {
	t.Helper()
	c, ok := got[name]
	if !ok {
		t.Fatalf("no check %q in %v", name, got)
	}
	if c.Skipped != skipped || (!skipped && c.Pass != pass) {
		t.Errorf("%s: pass %t, skipped %t (%s); want pass %t, skipped %t", name, c.Pass, c.Skipped, c.Why, pass, skipped)
	}
}

// Sheets as it should go: the plan says what the taf does, planque's grant is
// wide, and Coach says so before the card. Every check passes.
func TestASheetsPlanThatSaysItsSignInIsWiderPasses(t *testing.T) {
	got := graded(t, plans["sheets"], conversation(
		wrote("I'll set up a taf that reads your new order rows each Monday and tells you the total."),
		steps("Connect Google Sheets", "Agree the rules", "Start the taf"),
		showed(`{"type":"confirm","id":"c1","text":"Connect Google Sheets so the taf can read your rows?","options":[{"value":"yes","label":"Connect"}]}`),
		asked("grt_1", true),
		wrote("Google's sign-in asks for more access than the taf uses; the taf still uses only its own tools."),
		approvalFor("grt_1"),
	), true)
	for _, name := range []string{planReachesApproval, planNoReadOnlyWords, planNoHarmlessClaim, planWiderOnlyAfter} {
		wantPlan(t, got, name, true, false)
	}
}

// The Sheets proof's miss, in each place Coach writes: a step's title, a
// card's option, a message, in English and in French. The words fail
// whatever the judges say, and name where they were and the word, never
// Coach's line.
func TestReadOnlyFailsWhereverCoachWritesIt(t *testing.T) {
	for _, c := range []struct {
		name  string
		event session.Event
		says  string
	}{
		{"a step", steps("Connect Google Sheets, read-only", "Start the taf"), `a step says "read-only"`},
		{"a card", showed(`{"type":"confirm","id":"c1","text":"Connect Google Sheets?","note":"It changes nothing in your sheet."}`), `a card says "changes nothing"`},
		{"a message", wrote("The connection is read only."), `a message says "read only"`},
		{"a message, read only and a word after", wrote("It's read only access to your sheet."), `a message says "read only"`},
		{"a message, the job's words and then the claim", wrote("It will read only the new rows: the sign-in is read-only."), `a message says "read-only"`},
		{"a message, read only what it needs", wrote("The sign-in can read only what the taf needs."), `a message says "read only"`},
		{"an option, in French", showed(`{"type":"questions","id":"q1","questions":[{"id":"x","text":"Connecter Google Sheets ?","options":[{"value":"oui","label":"Oui, en lecture seule"}]}]}`), `a option says "lecture seule"`},
		{"a message, in French", wrote("La connexion ne modifie rien dans ton tableur."), `a message says "ne modifie rien"`},
		{"a message, won't change", wrote("It won't change anything."), `a message says "won't change anything"`},
	} {
		got := graded(t, plans["sheets"], conversation(c.event, asked("grt_1", true), wrote("Its sign-in asks for more access than the taf uses."), approvalFor("grt_1")), true)
		wantPlan(t, got, planNoReadOnlyWords, false, false)
		if why := got[planNoReadOnlyWords].Why; !strings.Contains(why, c.says) {
			t.Errorf("%s: %q, want it to say %s", c.name, why, c.says)
		}
	}
	// What the taf does is fine to say: it reads, it only tells.
	for _, fine := range []string{"The taf only reads your rows and tells you the total.", "Le taf lit seulement tes lignes.", "Read the new rows each Monday.",
		// What the taf reads, after "read only" (Eval 22b, 16 Nov): the job, not the sign-in.
		"It will read only the rows added since its last run, add up the Amount column, and post just that total."} {
		if found := readOnlyWordsIn([]planText{{where: "message", text: fine}}, nil); len(found) > 0 {
			t.Errorf("%q read as %v", fine, found)
		}
	}
}

// A claim in other words, or another language, is the judges'.
func TestAHarmlessClaimInOtherWordsIsJudged(t *testing.T) {
	got := graded(t, plans["sheets"], conversation(
		wrote("Keine Sorge, die Verbindung ist völlig harmlos."),
		asked("grt_1", true), wrote("It asks for more access than the taf uses."), approvalFor("grt_1"),
	), true, "harmlos")
	wantPlan(t, got, planNoReadOnlyWords, true, false)
	wantPlan(t, got, planNoHarmlessClaim, false, false)
}

// A wide sign-in Coach says nothing of passes: the card says it, from
// planque's grant (tafs cloud's wider.words), the same for every app.
func TestAWideSignInLeftToTheCardPasses(t *testing.T) {
	got := graded(t, plans["sheets"], conversation(
		steps("Connect Google Sheets"), asked("grt_1", true), wrote("Approve the connection to go on."), approvalFor("grt_1"),
	), true)
	for _, name := range []string{planReachesApproval, planNoReadOnlyWords, planNoHarmlessClaim, planWiderOnlyAfter} {
		wantPlan(t, got, name, true, false)
	}
}

// Wider said before planque answered fails, though it's said after too: Coach
// can't know it before.
func TestWiderBeforePlanqueAnsweredFails(t *testing.T) {
	got := graded(t, plans["sheets"], conversation(
		wrote("Google will ask for more access than the taf needs."),
		asked("grt_1", true), wrote("Its sign-in asks for more access than the taf uses."), approvalFor("grt_1"),
	), true)
	wantPlan(t, got, planWiderOnlyAfter, false, false)
}

// A narrowed sign-in (Gmail's, today): wider is never said, before or after
// planque answered. What planque said decides it, not the app.
func TestWiderForANarrowedSignInFails(t *testing.T) {
	for _, name := range []string{"gmail", "sheets"} {
		p := plans[name]
		ok := graded(t, p, conversation(steps("Connect it"), asked("grt_1", false), wrote("Approve it to go on."), approvalFor("grt_1")), false)
		wantPlan(t, ok, planWiderOnlyAfter, true, false)

		got := graded(t, p, conversation(steps("Connect it"), asked("grt_1", false),
			wrote("Its sign-in asks for more access than the taf uses."), approvalFor("grt_1")), false)
		wantPlan(t, got, planWiderOnlyAfter, false, false)
		if !strings.Contains(got[planWiderOnlyAfter].Why, "for a sign-in planque narrowed") {
			t.Errorf("%s why: %s", name, got[planWiderOnlyAfter].Why)
		}
	}
}

// A run that never asks to connect fails, and with no grant measures nothing
// of wider.
func TestAPlanThatNeverAsks(t *testing.T) {
	none := graded(t, plans["sheets"], conversation(wrote("Which spreadsheet is it?")), false)
	wantPlan(t, none, planReachesApproval, false, false)
	wantPlan(t, none, planWiderOnlyAfter, false, true)
}

// The texts are read from every place Coach writes, in order, up to the card;
// the card itself carries none of Coach's words.
func TestAPlanRunReadsEveryPlaceCoachWrites(t *testing.T) {
	r := planRunOf(conversation(
		wrote("one"),
		steps("two"),
		showed(`{"type":"questions","id":"q","questions":[{"id":"x","text":"three","note":"four","options":[{"value":"v","label":"five","description":"six"}]}]}`),
		showed(`{"type":"confirm","id":"c","title":"seven","text":"eight","yes":"nine","options":[{"value":"v","label":"ten"}]}`),
		asked("grt_other", true),
		asked("grt_1", true),
		approvalFor("grt_1"),
		wrote("eleven"),
	), "grt_1")
	var texts []string
	for _, x := range r.texts {
		texts = append(texts, x.where+":"+x.text)
	}
	want := "message:one step:two question:three question:four option:five option:six card:seven card:eight card:nine option:ten message:eleven"
	if strings.Join(texts, " ") != want {
		t.Errorf("texts:\n%s\nwant:\n%s", strings.Join(texts, " "), want)
	}
	if r.grantAt != 7 || r.cardAt != 8 {
		t.Errorf("grant at %d, card at %d; want 7 and 8 (another grant's answer isn't this one's)", r.grantAt, r.cardAt)
	}
	if b := r.before(r.grantAt); len(b) != 10 {
		t.Errorf("before planque answered: %v", b)
	}
}

// -plan is a case of its own, graded, in English or French.
func TestThePlanFlag(t *testing.T) {
	if p, err := planFlag("sheets", config{owner: "fr"}, true); err != nil || p.app != "Google Sheets" {
		t.Fatalf("sheets in French: %v %v", p, err)
	}
	for _, c := range []struct {
		name string
		cfg  config
		grad bool
	}{
		{"nope", config{owner: "en"}, true},
		{"sheets", config{owner: "es"}, true},
		{"sheets", config{owner: "en"}, false},
		{"gmail", config{owner: "en", desk: true}, true},
		{"gmail", config{owner: "en", stripeKey: "sk"}, true},
	} {
		if _, err := planFlag(c.name, c.cfg, c.grad); err == nil {
			t.Errorf("%s %+v graded %t: no error", c.name, c.cfg, c.grad)
		}
	}
	for name, p := range plans {
		for lang, w := range p.words {
			// The owner's own words never call it read-only: the case would grade its own ask.
			if found := readOnlyWordsIn([]planText{{where: "ask", text: w.ask + " " + w.facts + " " + w.yes}}, nil); len(found) > 0 {
				t.Errorf("%s %s: the owner says %v", name, lang, found)
			}
		}
	}
}

// The Drive proof (16 Nov): a later step was titled "Agree the rules:
// read-only file search", after the grant said its sign-in was wide. Next to a
// sign-in that can't be narrowed, a person reads "read-only" as the access, so a
// step title fails on it wherever it comes: the tool is named instead ("search
// your files").
func TestAReadOnlyStepTitleAfterAWideGrantFails(t *testing.T) {
	events := conversation(
		wrote("I'll set up a taf that reads your new order rows each Monday and tells you the total."),
		asked("grt_1", true),
		wrote("Google's sign-in asks for more access than the taf uses; the taf still uses only its own tools."),
		steps("Connect Google Sheets", "Agree the rules: read-only row search", "Start the taf"),
		approvalFor("grt_1"),
	)
	wantPlan(t, graded(t, plans["sheets"], events, true), planNoReadOnlyWords, false, false)
	// Named by what it does, the same plan passes (planque's request is two events: its call and its answer).
	events[4] = steps("Connect Google Sheets", "Agree the rules: search your rows", "Start the taf")
	wantPlan(t, graded(t, plans["sheets"], events, true), planNoReadOnlyWords, true, false)
}

// The setup definition's wording rules, as kiff/tafs sends en's and fr's, judge
// the same as the gate's own list: a tafs ref with them grades as one without.
func TestTheDefinitionsWordingJudgesAsTheGatesList(t *testing.T) {
	jobWords := make([]string, 0, len(readsOnly))
	for w := range readsOnly {
		jobWords = append(jobWords, w)
	}
	wording := []show.Wording{
		{Patterns: []string{`read-only|read only(\s+\pL+)?`, `(?:changes?|modif(?:y|ies)|touch(?:es)?) nothing`,
			`(?:won'?t|will not|doesn'?t|does not|can'?t|cannot|can not) (?:change|modify|touch) anything`},
			UnlessNext: jobWords, Fix: "say what the job does"},
		{Patterns: []string{`lecture seule`, `ne (?:change|modifie|touche)(?:ra)? rien`, `ne peut rien (?:changer|modifier)`, `aucune modification`},
			Fix: "dites ce que fait le taf"},
	}
	for _, text := range []string{
		"Connect Google Sheets, read-only", "It will read only the rows added since its last run.", "It can read only what it needs.",
		"Connecting it changes nothing.", "If you decline, nothing changes.", "It won't change anything in your account.",
		"Connexion en lecture seule.", "Elle ne changera rien.", "Aucune modification de votre compte.", "Read the new rows each morning.",
	} {
		own := readOnlyWordsIn([]planText{{where: "message", text: text}}, nil)
		def := readOnlyWordsIn([]planText{{where: "message", text: text}}, wording)
		if (len(own) > 0) != (len(def) > 0) {
			t.Errorf("%q: the gate's list says %v, the definition's rules %v", text, own, def)
		}
	}
}
