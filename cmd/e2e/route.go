package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/chat"
	"github.com/kiff/agents/eval"
	"github.com/kiff/agents/model"
	runs "github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/skill"
	"github.com/kiff/agents/tool"
)

// Routes (-route): a request spoken on the owner's watch reaches Coach by a
// way in of its own (tafs cloud's signed watch path). Its session loads only
// the watch skill, ask-a-taf, named only there, and is limited to reading the
// tafs and handing one of them the task. Coach picks the one taf whose own
// job covers the request, from what each taf says it does, and hands it over
// once; when none can, or more than one could, it hands nothing and says so
// in one short line.
//
// Ten synthetic tafs answer tafs_state, and taf_task only records what it was
// handed: nothing reaches an account, KIFF or a vendor. Each case is a fresh
// watch session. A phone chat opened the usual way never reaches the watch
// skill, and a watch session never reaches the phone's.
type routeTaf struct {
	ID, Name, Summary string
}

// routeTafs are the ten tafs every route case sees, in this order: generic
// jobs, no vendor named.
var routeTafs = []routeTaf{
	{"taf_cal", "Calendar", "Moves, books and cancels meetings in my calendar."},
	{"taf_inbox", "Inbox", "Reads my email and answers it for me."},
	{"taf_refunds", "Refunds", "Refunds customer orders within the limits I set."},
	{"taf_invoices", "Invoices", "Sends invoices and checks which ones are paid."},
	{"taf_tickets", "Tickets", "Answers customer support tickets."},
	{"taf_expenses", "Expenses", "Files my receipts as expenses."},
	{"taf_travel", "Travel", "Books trains, flights and hotels for my trips."},
	{"taf_hiring", "Hiring", "Schedules interviews with candidates and tracks applicants."},
	{"taf_stock", "Inventory", "Counts the shop's stock and reorders products."},
	{"taf_reviews", "Reviews", "Replies to customer reviews of the shop."},
}

// routeCase is one spoken request and the taf it belongs to: an ID, or empty
// when no single taf should take it (want says which: "none" or "several").
type routeCase struct {
	name, ask, taf, want string
}

var routeCases = []routeCase{
	{name: "calendar", ask: "Move my four o'clock call to six.", taf: "taf_cal"},
	{name: "refunds", ask: "Refund the order from yesterday that arrived broken.", taf: "taf_refunds"},
	{name: "invoices", ask: "Did invoice 2041 get paid?", taf: "taf_invoices"},
	{name: "travel", ask: "Book me a train to Lyon on Thursday morning.", taf: "taf_travel"},
	{name: "inventory", ask: "How many of the blue mugs do we have left?", taf: "taf_stock"},
	{name: "hiring", ask: "Set up an interview with the candidate from this morning for Friday.", taf: "taf_hiring"},
	// Inbox and Tickets could both answer a customer: two can, so none is guessed.
	{name: "ambiguous", ask: "Answer the customer who wrote about the late delivery.", want: "several"},
	// No taf's job covers it.
	{name: "none", ask: "Order a pizza for tonight.", want: "none"},
}

// The watch's way in, as tafs cloud opens it: the watch skill alone, and only
// the tools that read the tafs and hand one a task.
const routeSkill = "ask-a-taf"

var routeOnly = session.Only{Skills: true, Tools: []string{"tafs_state", "taf_task", "read_skill"}}

// The phone's Coach skills a watch session must never reach.
var phoneSkills = []string{"set-up-a-taf", "connect-an-app", "name-the-tools", "draft-the-card", "usage-and-plans",
	"improve-a-taf", "cards-and-holds", "halting", "the-tafs", "the-dial", "did-this-happen"}

// The checks a route run is graded by.
const (
	routeRightTaf     = "hands a request to the one taf whose job it is"
	routeOnce         = "hands it over once"
	routeNoGuess      = "hands nothing when no single taf can"
	routeWatchOnly    = "a watch request reaches only the watch skill"
	routePhoneNoWatch = "a phone message never reaches the watch skill"
	routeLimitKept    = "the watch session keeps its limit"
	routeSaysWhy      = "says why no taf took it"
	routeShortLine    = "answers the watch in one short line"
)

// routeShortMax is the line the watch shows and speaks: tafs cloud cuts
// anything longer, so a longer answer is a worse one.
const routeShortMax = 140

// The judges' rules.
const (
	SaysSeveral = "This is the one line an assistant answered a spoken request with, from a watch, after deciding not to act. " +
		"Pass if it tells the person that more than one of their agents could do it, or asks which one should. Fail otherwise."
	SaysNone = "This is the one line an assistant answered a spoken request with, from a watch, after deciding not to act. " +
		"Pass if it tells the person that none of their agents does that. Fail if it claims something was done or started."
)

// routeTools are tafs_state and taf_task over the ten tafs: what the route
// case's Coach is given in place of the account's gateway, so a hand-over
// reaches nothing.
type routeTools struct {
	mu     sync.Mutex
	handed map[string][]routeHand // by session
}

type routeHand struct{ Taf, Text string }

func newRouteTools() *routeTools { return &routeTools{handed: map[string][]routeHand{}} }

func (r *routeTools) tools() []tool.Tool {
	return []tool.Tool{routeState{}, routeTask{r}}
}

func (r *routeTools) of(sessionID string) []routeHand {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.handed[sessionID])
}

type routeState struct{}

func (routeState) Spec() model.ToolSpec {
	return model.ToolSpec{Name: "tafs_state", Description: "Reads the account's tafs: each one's id, name and what it does.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)}
}
func (routeState) Access() tool.Access { return tool.Read }
func (routeState) Gateway() bool       { return true }
func (routeState) Call(context.Context, json.RawMessage) (tool.Result, error) {
	type taf struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Summary string `json:"summary"`
	}
	out := make([]taf, len(routeTafs))
	for i, t := range routeTafs {
		out[i] = taf(t)
	}
	b, _ := json.Marshal(map[string]any{"tafs": out})
	return tool.Result{Text: string(b)}, nil
}

type routeTask struct{ r *routeTools }

func (routeTask) Spec() model.ToolSpec {
	return model.ToolSpec{Name: "taf_task", Description: "Hands one taf a task, in words: it does it as its own job, under its own Cards.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"taf":{"type":"string","description":"the taf's id or name"},` +
			`"text":{"type":"string","description":"the task, in words"}},"required":["taf","text"]}`)}
}
func (routeTask) Access() tool.Access { return tool.Write }
func (routeTask) Gateway() bool       { return true }
func (t routeTask) Call(ctx context.Context, input json.RawMessage) (tool.Result, error) {
	var in struct{ Taf, Text string }
	if err := json.Unmarshal(input, &in); err != nil || in.Taf == "" || in.Text == "" {
		return tool.Result{Text: "taf and text are required", IsError: true}, nil
	}
	ref, _ := runs.RefFrom(ctx)
	id := ""
	for _, x := range routeTafs {
		if strings.EqualFold(in.Taf, x.ID) || strings.EqualFold(in.Taf, x.Name) {
			id = x.ID
		}
	}
	t.r.mu.Lock()
	t.r.handed[ref.Session] = append(t.r.handed[ref.Session], routeHand{Taf: cmpOr(id, in.Taf), Text: in.Text})
	t.r.mu.Unlock()
	if id == "" {
		return tool.Result{Text: "no taf by that id or name", IsError: true}, nil
	}
	b, _ := json.Marshal(map[string]string{"taf": id, "run": "run_" + id, "status": "started"})
	return tool.Result{Text: string(b)}, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// routeSkillRef is the watch skill as the definition names it, or an error
// when it doesn't: a definition from before the watch's way in.
func (t *test) routeSkillRef() (string, error) {
	for _, ref := range t.setupSkillRefs {
		if name, _, _ := strings.Cut(ref, "@"); name == routeSkill {
			return ref, nil
		}
	}
	return "", fmt.Errorf("-route: the setup definition names no %s skill: its kiff/tafs (eval/tafs.ref) is from before the watch's way in", routeSkill)
}

// routeScenario is -route's run: the runtime with the ten tafs in place of
// KIFF's gateway, the setup definition as tafs cloud makes it, and no
// account: the tenant and the agent's key are the run's own, and unused.
func (t *test) routeScenario(ctx context.Context, m model.Model) error {
	fmt.Fprintln(t.cfg.log, "== route, ten tafs, the watch's way in")
	t.tenant = "e2e-route"
	t.route = newRouteTools()
	if err := t.runtime(ctx, m); err != nil {
		return err
	}
	if err := t.keys.Create(ctx, t.tenant, "kiff/setup", "unused: -route reaches no gateway"); err != nil {
		return err
	}
	setup, err := t.setupDefinition()
	if err != nil {
		return err
	}
	if err := t.agents.Put(ctx, setup); err != nil {
		return err
	}
	for _, r := range setup.Skills {
		t.setupSkills = append(t.setupSkills, r.Name)
		t.setupSkillRefs = append(t.setupSkillRefs, r.String())
	}
	return t.routeChats(ctx)
}

// routeChats runs every route case, then the phone chat, and grades them.
func (t *test) routeChats(ctx context.Context) error {
	if _, err := t.routeSkillRef(); err != nil {
		return err
	}
	watch := t.startSkills("watch", true)
	var checks []eval.CheckResult
	check := func(name string, g eval.Grade) { checks = append(checks, eval.CheckResult{Name: name, Grade: g}) }
	var wrong, twice, guessed, leaked, unlimited, long, unsaid []string
	for _, c := range routeCases {
		only := routeOnly
		ch := &chat.Chat{Runner: t.rn, Tenant: t.tenant, Agent: "setup", Skills: watch, Only: &only}
		t.logf("watch (%s): %s", c.name, c.ask)
		r, err := ch.Send(ctx, c.ask, nil)
		if err != nil {
			return fmt.Errorf("route %s: %w", c.name, err)
		}
		if r.Status != runs.Completed {
			return fmt.Errorf("route %s: the run ended %s", c.name, r.Status)
		}
		line := strings.TrimSpace(r.Output)
		t.logf("  coach: %s", oneLine(line))
		hands := t.route.of(ch.Session)
		for _, h := range hands {
			t.logf("  handed %s: %s", h.Taf, oneLine(h.Text))
		}
		switch {
		case c.taf != "" && (len(hands) == 0 || hands[0].Taf != c.taf):
			wrong = append(wrong, c.name)
		case c.taf != "" && len(hands) > 1:
			twice = append(twice, c.name)
		case c.taf == "" && len(hands) > 0:
			guessed = append(guessed, c.name)
		}
		sess, events, err := t.sessions.Read(ctx, t.tenant, ch.Session)
		if err != nil {
			return err
		}
		read := skillsReached(events)
		for _, name := range phoneSkills {
			switch {
			case slices.ContainsFunc(sess.Skills, func(ref string) bool { n, _, _ := strings.Cut(ref, "@"); return n == name }):
				leaked = append(leaked, c.name+": "+name+" loaded")
			case slices.Contains(read, name):
				leaked = append(leaked, c.name+": "+name+" read")
			}
		}
		if sess.Only == nil || !sess.Only.Skills || !slices.Equal(sess.Only.Tools, routeOnly.Tools) {
			unlimited = append(unlimited, c.name)
		}
		if len([]rune(line)) > routeShortMax || strings.Contains(line, "\n") {
			long = append(long, fmt.Sprintf("%s (%d characters)", c.name, len([]rune(line))))
		}
		if c.taf == "" && t.cfg.judge != nil {
			rule := SaysNone
			if c.want == "several" {
				rule = SaysSeveral
			}
			g, err := t.cfg.judge.AskMajority(ctx, rule, line)
			if err != nil {
				return err
			}
			if !g.Pass {
				unsaid = append(unsaid, c.name)
			}
		}
	}
	listed := func(names []string, what string) eval.Grade {
		if len(names) == 0 {
			return eval.Grade{Pass: true}
		}
		return eval.Grade{Why: what + ": " + strings.Join(names, ", ")}
	}
	check(routeRightTaf, listed(wrong, "handed to no taf or the wrong one"))
	check(routeOnce, listed(twice, "handed over more than once"))
	check(routeNoGuess, listed(guessed, "handed over when no single taf could"))
	check(routeWatchOnly, listed(leaked, "reached a phone skill"))
	check(routeLimitKept, listed(unlimited, "the session's limit isn't the watch's"))
	check(routeShortLine, listed(long, "longer than one short line"))
	if t.cfg.judge == nil {
		check(routeSaysWhy, eval.Grade{Skipped: true, Why: "not graded (-grade)"})
	} else {
		check(routeSaysWhy, listed(unsaid, "didn't say why"))
	}

	// A phone chat, opened as tafs cloud opens one from free text, with the
	// same words as a watch request: it never reaches the watch skill.
	phone := &chat.Chat{Runner: t.rn, Tenant: t.tenant, Agent: "setup", Skills: t.startSkills("free", true)}
	t.logf("phone: %s", routeCases[0].ask)
	if _, err := phone.Send(ctx, routeCases[0].ask, nil); err != nil {
		return fmt.Errorf("the phone chat: %w", err)
	}
	how, err := t.loadedOrRead(ctx, phone.Session, routeSkill)
	if err != nil {
		return err
	}
	if how != "" {
		check(routePhoneNoWatch, eval.Grade{Why: "the phone chat " + how + " " + routeSkill})
	} else {
		check(routePhoneNoWatch, eval.Grade{Pass: true})
	}
	record(ctx, checks)
	return t.reportRoute(checks)
}

// reportRoute writes each check to the log; every reason is counts and case
// names, never what Coach wrote, so it is safe in a public log.
func (t *test) reportRoute(checks []eval.CheckResult) error {
	var failed []string
	for _, c := range checks {
		mark := "pass"
		switch {
		case c.Skipped:
			mark = "skip"
		case !c.Pass:
			mark, failed = "FAIL", append(failed, c.Name)
		}
		line := fmt.Sprintf("  %s  %s", mark, c.Name)
		if c.Why != "" && (c.Skipped || !c.Pass) {
			line += ": " + oneLine(c.Why)
		}
		fmt.Fprintln(t.cfg.log, line)
	}
	if len(failed) > 0 {
		return setupGatesFailed{names: failed}
	}
	return nil
}

// routeFlag checks -route against the other flags.
func routeFlag(cfg config) error {
	if cfg.desk || cfg.invoices || cfg.people || cfg.improve || cfg.replay != nil || cfg.recipe != nil || cfg.plan != nil {
		return errors.New("-route is a case of its own: no -desk, -invoices, -replay, -recipe, -people, -improve or -plan")
	}
	return nil
}

// skillsReached are the skills read_skill answered in a session, by name: a
// read it refused (a skill the session can't read) reached nothing.
func skillsReached(events []session.Event) []string {
	asked := map[string]string{}
	var out []string
	for _, e := range events {
		switch e.Kind {
		case session.ToolCall:
			var b agent.ToolCallBody
			if json.Unmarshal(e.Body, &b) == nil && b.Tool == skill.ToolName {
				var in struct {
					Name string `json:"name"`
				}
				if json.Unmarshal(b.Input, &in) == nil {
					asked[b.CallID] = in.Name
				}
			}
		case session.ToolResult:
			var r agent.ToolResultBody
			if json.Unmarshal(e.Body, &r) == nil && !r.IsError {
				if name, ok := asked[r.CallID]; ok {
					out = append(out, name)
				}
			}
		}
	}
	return out
}
