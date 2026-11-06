// Package show is the tool through which an agent shows the owner a UI
// component in the chat: its plan as steps, a notice, a question, an
// approval, a vendor sign-in, or the result (kiff/agents#91, contract 1).
// The run records each one as a component event, kept like the agent's
// messages, and the app draws it.
//
// A component grants nothing. An approval names planque's grant and KIFF's
// draft, which the app reads from those services and the phone signs there;
// the agent learns the outcome from planque and KIFF, never from the chat.
package show

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/tool"
)

// Name is the tool's name, as the model sees it.
const Name = "show"

// Limits on one component.
const (
	MaxText    = 2000
	MaxLabel   = 200
	MaxSteps   = 20
	MaxOptions = 4
	MaxTools   = 20
	// A questions card asks 1 to MaxQuestions questions at once, each with
	// 2 to MaxQuestionOptions options (kiff/tafs#138 item 57).
	MaxQuestions       = 4
	MaxQuestionOptions = 5
)

// Types are the components the app draws, the ones an agent shows unless
// its definition names others (New).
var Types = []string{"steps", "notice", "confirm", "questions", "approval", "sign_in", "result", "knob", "drive"}

// AskCoach is a taf's card for a setting only Coach changes: a line, and an
// Ask Coach button that opens Coach on the taf with the owner's request
// written in (kiff/tafs#476, gap 15). It waits for nothing.
const AskCoach = "ask_coach"

// TafTypes are the cards a definition may name for its agent alone, in
// place of Types (registry Definition.ShowTypes).
var TafTypes = []string{AskCoach}

// MaxAsk bounds an ask_coach card's request.
const MaxAsk = 500

var (
	idPattern  = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)
	refPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
	statuses   = []string{"pending", "active", "done", "failed", "skipped"}
	tones      = []string{"info", "success", "warning", "error"}
)

// Step is one line of a plan. Call, when set, names the tool call the step
// waits on: a step whose call this run made and that was refused, held, not
// made or failed can't be done (stepDone).
type Step struct {
	Label  string `json:"label"`
	Status string `json:"status"`
	Call   string `json:"call,omitempty"`
}

// Option is one answer to a confirm or a question.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
	// Description says more about the option, under its label: only in a
	// question.
	Description string `json:"description,omitempty"`
}

// Question is one of a questions card's questions: its own id, its text,
// its options, whether several may be picked (Multi), and a line under it
// (Note). The app adds a field for the owner's own words under each, and
// "Chat about this" on the card: never the agent's to send.
type Question struct {
	ID      string   `json:"id"`
	Text    string   `json:"text"`
	Multi   bool     `json:"multi,omitempty"`
	Options []Option `json:"options"`
	Note    string   `json:"note,omitempty"`
}

// Taf is the taf a result hands over to: what tafs cloud starts it from
// (kiff/tafs#50, POST /v1/tafs with from_setup), or, with Improves, the next
// version of a taf tafs already runs (kiff/tafs#107).
type Taf struct {
	Agent string `json:"agent"`
	Name  string `json:"name"`
	// Job is the taf's job: for a new taf, empty or "custom" (its own job,
	// drafted with the owner: Kickoff, Instructions, Cron or Unscheduled;
	// kiff/tafs#476); for an improvement, the taf's job as tafs has it.
	Job string `json:"job,omitempty"`
	// Draft is the taf's KIFF draft: for a new taf, the one the owner
	// confirmed in the second approval; for an improvement, the revision of
	// its Cards, which the owner confirms only when they switch to the
	// version. An improvement that changes no Card has none.
	Draft string `json:"draft,omitempty"`
	// Improves is the tafs id of the taf this is the next version of.
	Improves string `json:"improves,omitempty"`
	// Note says what changed, in the owner's language (an improvement).
	Note string `json:"note,omitempty"`
	// Summary is one short line on what the taf does, in the owner's
	// language: its job line once the taf, or this version, goes live.
	Summary string `json:"summary,omitempty"`
	// Notes are the taf's instructions from the owner for the new version,
	// in full; empty keeps them as they are (an improvement).
	Notes string `json:"notes,omitempty"`
	// Unscheduled says a new taf's tools can't find its work by themselves,
	// so it gets no schedule: the owner hands it work in its chat
	// (kiff/tafs#138). An improvement keeps the taf's schedule.
	Unscheduled bool `json:"unscheduled,omitempty"`
	// Kickoff is what each run of a new taf is asked to do, in one or two
	// sentences; Instructions how it works, as agreed with the owner; Cron
	// when it runs (five POSIX fields, in the account's time zone), unless
	// it is Unscheduled. A new taf's job of its own (kiff/tafs#476).
	Kickoff      string `json:"kickoff,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	Cron         string `json:"cron,omitempty"`
	// Disclose names a taf's tools that write to someone outside the
	// company, each with the free-text fields of its input (dotted paths):
	// tafs puts them in the taf's definition, and the runtime ends each such
	// call with its AI disclosure (agent.Disclosure, kiff/tafs#476). An
	// improvement's adds to the taf's: a tool named again adds its fields,
	// and none is ever taken away.
	Disclose []Disclose `json:"disclose,omitempty"`
	// WritesFor is the name the taf writes for, as the owner answered who
	// its messages come from (the account's company unless they said
	// otherwise): its disclosure names it ("Written by an AI agent for
	// Kiff Agents OÜ."), one line (kiff/tafs#476, gaps 16 and 18).
	WritesFor string `json:"writes_for,omitempty"`
}

// MaxWritesFor bounds the name a taf writes for.
const MaxWritesFor = 100

// Disclose is one tool of a result's taf.disclose: its name in KIFF, and the
// paths of the fields its disclosure goes in, in order of preference.
type Disclose struct {
	Tool   string   `json:"tool"`
	Fields []string `json:"fields"`
	// Credit false leaves "Built with tafs." out of the tool's calls, which
	// carry the disclosure alone: evidence or a filing, for a bank or a
	// card network. Left out, a message to people carries both (the owner's
	// call, kiff/tafs#476).
	Credit *bool `json:"credit,omitempty"`
}

// MaxDisclose bounds a result's taf.disclose: tools, each one's fields, and
// a name's or a path's length.
var MaxDisclose = struct{ Tools, Fields, Length int }{10, 5, 120}

// checkDisclose says what's wrong with a result's taf.disclose, if anything.
func checkDisclose(ds []Disclose) error {
	if len(ds) > MaxDisclose.Tools {
		return fmt.Errorf("taf.disclose names at most %d tools", MaxDisclose.Tools)
	}
	for _, d := range ds {
		if d.Tool == "" || len(d.Tool) > MaxDisclose.Length {
			return fmt.Errorf("each of taf.disclose names its tool as KIFF names it")
		}
		if len(d.Fields) == 0 || len(d.Fields) > MaxDisclose.Fields {
			return fmt.Errorf("each of taf.disclose has 1 to %d fields", MaxDisclose.Fields)
		}
		for _, f := range d.Fields {
			if f == "" || len(f) > MaxDisclose.Length || slices.Contains(strings.Split(f, "."), "") {
				return fmt.Errorf("a field of taf.disclose is a dotted path, such as body or evidence.uncategorized_text, not %q", f)
			}
		}
	}
	return nil
}

// writesFor says what's wrong with a result's taf.writes_for, if anything:
// a name on one line, at most MaxWritesFor characters.
func writesFor(s string) error {
	if s != "" && (strings.TrimSpace(s) != s || strings.ContainsFunc(s, unicode.IsControl)) {
		return fmt.Errorf("taf.writes_for is a name on one line, such as the account's company")
	}
	return text("taf.writes_for", s, false, MaxWritesFor)
}

// MaxNotes bounds an improvement's instructions from the owner.
const MaxNotes = 4000

// MaxJob bounds a new taf's kickoff and its instructions, as tafs does.
const MaxJob = 2000

// cronPattern is five POSIX cron fields; tafs checks how often it runs.
var cronPattern = regexp.MustCompile(`^[0-9*/,-]+( [0-9*/,-]+){4}$`)

// Component is one component. Which fields it has depends on its type.
type Component struct {
	Type    string   `json:"type"`
	ID      string   `json:"id"`
	Title   string   `json:"title,omitempty"`
	Text    string   `json:"text,omitempty"`
	Tone    string   `json:"tone,omitempty"`
	Steps   []Step   `json:"steps,omitempty"`
	Options []Option `json:"options,omitempty"`
	// Questions are a questions card's questions, answered together.
	Questions []Question `json:"questions,omitempty"`
	Grant     string     `json:"grant,omitempty"`
	Draft     string     `json:"draft,omitempty"`
	Service   string     `json:"service,omitempty"`
	Taf       *Taf       `json:"taf,omitempty"`
	Tools     []string   `json:"tools,omitempty"`
	// The card's own words for its buttons and the line under them, in the
	// owner's language (kiff/tafs#75). Yes is the main button, No the other,
	// Note the line below. A card without them shows the app's words. Never
	// on an approval: the owner's tap there grants, so its words are the
	// app's, in the phone's language.
	Yes  string `json:"yes,omitempty"`
	No   string `json:"no,omitempty"`
	Note string `json:"note,omitempty"`
	// TafID and Version are a dial card's (knob): the taf's tafs id and the
	// dial version Coach drafted for it. Its words are tafs cloud's, read by
	// the app from the taf's dial, never the agent's.
	TafID   string `json:"taf_id,omitempty"`
	Version int    `json:"version,omitempty"`
	// Waits says the run pauses on this card until it's answered, and who
	// answers it: WaitsOwner (Answerable: the person) or WaitsService
	// (ByService: the service that hands the owner over). The show tool sets
	// it, never the agent, and a card the run goes on past (steps, notice,
	// knob) has none. tafs cloud and the apps read it and keep no list of their
	// own, which drifted apart (kiff/tafs, the drive card placed before Coach's
	// words on Android).
	Waits string `json:"waits,omitempty"`
}

// Who a card's run waits on (Component.Waits).
const (
	WaitsOwner   = "owner"
	WaitsService = "service"
)

// waitsOn is who the run waits on for a card of type t, or "" when it goes on.
func waitsOn(t string) string {
	switch {
	case !slices.Contains(Answerable, t):
		return ""
	case slices.Contains(ByService, t):
		return WaitsService
	}
	return WaitsOwner
}

// fields lists the fields each type may carry, besides type and id.
var fields = map[string][]string{
	"steps":     {"title", "steps"},
	"notice":    {"tone", "title", "text"},
	"confirm":   {"text", "options"},
	"questions": {"text", "questions"},
	"approval":  {"grant", "draft"}, // no agent words: the owner's tap grants, so its text is planque's and the app's
	"sign_in":   {"grant", "service", "title", "text", "yes", "no", "note"},
	"result":    {"taf", "text", "tools", "yes"},
	"knob":      {"taf_id", "version"}, // no agent words: the app draws the dial's draft as tafs cloud words it
	"drive":     {"text"},              // the request, in the owner's words: the app's approval prompt names it (kiff/tafs#544)
	AskCoach:    {"text"},              // the owner's request for Coach, in their words: the app writes it into Coach's box
}

// Tool shows components: those of types, or Types when it names none.
type Tool struct{ types []string }

// New returns the show tool, for types alone when given (a taf's
// ask_coach); with none, for Types, as Coach has it.
func New(types ...string) Tool { return Tool{types: types} }

// allowed are the types t shows.
func (t Tool) allowed() []string {
	if len(t.types) == 0 {
		return Types
	}
	return t.types
}

// Access: showing changes only the agent's own chat.
func (Tool) Access() tool.Access { return tool.Own }
func (Tool) Gateway() bool       { return false }

// Shows: the show tool's components go to the owner's chat.
func (Tool) Shows() bool { return true }

func (t Tool) Spec() model.ToolSpec {
	if len(t.types) > 0 {
		return t.tafSpec()
	}
	return model.ToolSpec{
		Name: Name,
		Description: "Show the owner a component in the chat; the app draws it. Showing an id again replaces that component in place (to advance steps). " +
			"steps {title, steps:[{label, status: pending|active|done|failed|skipped, call?}]}, where call names the tool a step waits on, and the step is done only once that call went through; notice {tone: info|success|warning|error, title?, text}; " +
			"confirm {text, options:[{value, label}], 2 to 4}: the owner's choice comes back as their next message, \"[confirm <id>] <value>\"; " +
			"questions {text, questions:[{id, text, options:[{value, label, description?}], 2 to 5, multi?, note?}], 1 to 4}: several questions the owner answers together, when none depends on another's answer; " +
			"the app adds a field for the owner's own words under each, and Chat about this; the answers come back as one message, " +
			"[questions <id>] then a JSON object from each question's id to {value}, {values} (multi) or {own} (the owner's own words); " +
			"approval {grant, draft?}: the app shows planque's grant and KIFF's draft as those services say, and the owner approves with Face ID; " +
			"sign_in {grant, service}: the owner signs in at the vendor again; " +
			"knob {taf_id, version}: the dial version you drafted for a taf (taf_dial's answer), which the app draws from tafs cloud's words, with the switch to it; " +
			"drive {text}: ask to drive the app on the owner's phone for one request, text naming it in their words; they approve it with their fingerprint, and the answer is \"approved: the drive is on\" or \"declined\"; result {taf:{agent, name, job, draft, summary?}, text, tools}: hand the owner over to the taf, with its job (one of tafs' jobs, such as refunds or access) and the KIFF draft the owner confirmed; summary is one short line on what the taf does, in the owner's language; " +
			"to improve a taf instead, result {taf:{agent, name, job, improves, note, notes?, draft?, summary?}, text, tools}: improves is the taf's tafs id, note what changed, notes its full new instructions from the owner (omit to keep them), " +
			"and draft the KIFF draft revising its Cards, which the owner confirms only when they switch to the new version, never in this chat. " +
			"A result waits while the taf or its version is made for the owner; what came of it is the call's result: {taf_id} (set up), {version} (the new version) or {failed} (why not): then say why, and hand over again once it's fixed. " +
			"The app keeps no words of its own on a card: a sign_in takes title, text, yes (the button that opens the vendor's page), no (e.g. Not now) and note; a result takes yes (the button that opens the taf). " +
			"Write them in the owner's language. An approval takes no words of yours: what it grants, and its buttons, are planque's, KIFF's and the app's. " +
			"A component grants nothing: after an approval, check the grant's status in planque before going on.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"type":{"type":"string","enum":["steps","notice","confirm","questions","approval","sign_in","result","knob","drive"]},` +
			`"id":{"type":"string","description":"1 to 64 of a-z 0-9 - _; the same id replaces the component"},` +
			`"title":{"type":"string"},"text":{"type":"string"},` +
			`"tone":{"type":"string","enum":["info","success","warning","error"]},` +
			`"steps":{"type":"array","items":{"type":"object","properties":{"label":{"type":"string"},"status":{"type":"string","enum":["pending","active","done","failed","skipped"]},` +
			`"call":{"type":"string","description":"the tool the step waits on, by name: done only once that call went through"}},"required":["label","status"]}},` +
			`"options":{"type":"array","items":{"type":"object","properties":{"value":{"type":"string"},"label":{"type":"string"}},"required":["value","label"]}},` +
			`"questions":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"text":{"type":"string"},"multi":{"type":"boolean"},` +
			`"options":{"type":"array","items":{"type":"object","properties":{"value":{"type":"string"},"label":{"type":"string"},"description":{"type":"string"}},"required":["value","label"]}},` +
			`"note":{"type":"string"}},"required":["id","text","options"]}},` +
			`"grant":{"type":"string","description":"planque's grant id"},"draft":{"type":"string","description":"KIFF's agent draft id"},` +
			`"service":{"type":"string","description":"the vendor's host: its MCP server's, or its API's base"},` +
			`"taf":{"type":"object","properties":{"agent":{"type":"string"},"name":{"type":"string"},` +
			`"job":{"type":"string","description":"an improvement only: the taf's job as tafs has it; a new taf leaves it out, its job is its own (kickoff, instructions, cron)"},"draft":{"type":"string","description":"the taf's KIFF draft id: confirmed by the owner for a new taf; an improvement's Card revision, confirmed at the switch"},` +
			`"improves":{"type":"string","description":"the tafs id of the taf this is the next version of"},` +
			`"note":{"type":"string","description":"what changed, in the owner's language (an improvement)"},` +
			`"notes":{"type":"string","description":"the taf's full new instructions from the owner (an improvement); omit to keep them"},` +
			`"summary":{"type":"string","description":"one short line on what the taf does, in the owner's language: its job line once it goes live"},` +
			`"unscheduled":{"type":"boolean","description":"a new taf whose tools can't find its work by themselves: no schedule, the owner hands it work in its chat"},` +
			`"kickoff":{"type":"string","description":"a new taf: what each run is asked to do, in one or two sentences, drafted with the owner"},` +
			`"instructions":{"type":"string","description":"a new taf: how it works, from what was agreed with the owner, and its never rules"},` +
			`"cron":{"type":"string","description":"a new taf with a schedule: when it runs, five POSIX cron fields in the account's time zone, such as 0 9 * * 1-5"},` +
			`"disclose":{"type":"array","description":"each of the taf's tools that writes to someone outside the company (an email, a reply, evidence), by its name in KIFF, with its free-text fields' dotted paths: each such call then ends with its AI disclosure; an improvement's adds to the taf's",` +
			`"items":{"type":"object","properties":{"tool":{"type":"string"},"fields":{"type":"array","items":{"type":"string"}},` +
			`"credit":{"type":"boolean","description":"false for evidence or a filing, to a bank or a card network: the disclosure alone, without \"Built with tafs.\"; leave it out for messages to people"}},"required":["tool","fields"]}},` +
			`"writes_for":{"type":"string","description":"who the taf's messages come from, as the owner answered (the account's company unless they said otherwise): its disclosure names it"}},` +
			`"required":["agent","name"]},` +
			`"tools":{"type":"array","items":{"type":"string"}},` +
			`"yes":{"type":"string","description":"the main button's label (sign_in, result)"},` +
			`"no":{"type":"string","description":"the other button's label (sign_in)"},` +
			`"note":{"type":"string","description":"the line under the buttons (sign_in)"},` +
			`"taf_id":{"type":"string","description":"the taf's tafs id (knob)"},` +
			`"version":{"type":"integer","description":"the dial version taf_dial made (knob)"}},` +
			`"required":["type","id"]}`),
	}
}

// Call checks the component and returns it to be recorded. A component that
// doesn't fit is an error result naming what to fix.
func (t Tool) Call(ctx context.Context, input json.RawMessage) (tool.Result, error) {
	c, err := Parse(input)
	if err != nil {
		return tool.Result{Text: err.Error(), IsError: true}, nil
	}
	if !slices.Contains(t.allowed(), c.Type) {
		return tool.Result{Text: "type must be one of " + strings.Join(t.allowed(), ", "), IsError: true}, nil
	}
	if err := stepsDone(c, tool.OutcomesFrom(ctx)); err != nil {
		return tool.Result{Text: err.Error(), IsError: true}, nil
	}
	c.Waits = waitsOn(c.Type)
	b, _ := json.Marshal(c)
	if slices.Contains(Answerable, c.Type) {
		// The run waits here: the owner's answer is this call's result.
		who := "the owner's answer"
		if slices.Contains(ByService, c.Type) {
			who = "what came of the hand-over"
		}
		return tool.Result{Text: "shown: " + c.Type + " " + c.ID + ", waiting for " + who, Component: b, State: tool.StateAwaiting}, nil
	}
	return tool.Result{Text: "shown: " + c.Type + " " + c.ID, Component: b}, nil
}

// clean takes any tag of the model's format out of the words the owner reads on
// the card, before it's checked, recorded and shown: its title, text, note,
// buttons, steps, options, questions and a result's taf words (Eval
// 42141761407; qa-kiff on #345). Values, ids and references are left as they
// are. Parse does it, so the words recorded with the owner's answer, which the
// server takes from the card it parses again, are the ones the owner saw.
func (c *Component) clean() {
	s := model.StripLeakedTags
	c.Title, c.Text, c.Note, c.Yes, c.No = s(c.Title), s(c.Text), s(c.Note), s(c.Yes), s(c.No)
	for i := range c.Steps {
		c.Steps[i].Label = s(c.Steps[i].Label)
	}
	cleanOptions := func(os []Option) {
		for i := range os {
			os[i].Label, os[i].Description = s(os[i].Label), s(os[i].Description)
		}
	}
	cleanOptions(c.Options)
	for i := range c.Questions {
		c.Questions[i].Text, c.Questions[i].Note = s(c.Questions[i].Text), s(c.Questions[i].Note)
		cleanOptions(c.Questions[i].Options)
	}
	if t := c.Taf; t != nil {
		t.Name, t.Summary, t.Note, t.Notes = s(t.Name), s(t.Summary), s(t.Note), s(t.Notes)
		t.Kickoff, t.Instructions = s(t.Kickoff), s(t.Instructions)
	}
}

// tafSpec is the show tool for a taf's own cards: ask_coach alone, today.
func (t Tool) tafSpec() model.ToolSpec {
	return model.ToolSpec{
		Name: Name,
		Description: "Show the owner a card in your chat; the app draws it. " +
			"ask_coach {id, text}: when the owner asks you to change a setting (connect an app or give it a new key, change your Card, your tools or your schedule), " +
			"that's Coach's to do, never yours: say so in one line, and show this card with their request, in their words, as text. " +
			"The app's Ask Coach button opens Coach on you with it written in, for the owner to send. It waits for nothing.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"type":{"type":"string","enum":` + mustJSON(t.types) + `},` +
			`"id":{"type":"string","description":"1 to 64 of a-z 0-9 - _"},` +
			`"text":{"type":"string","description":"the owner's request for Coach, in their words and language"}},` +
			`"required":["type","id","text"]}`),
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// Parse checks one component against contract 1 and returns it, cleaned of
// leaked tags: a field that held only a tag is then empty, and checked so.
func Parse(input json.RawMessage) (Component, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(input, &raw); err != nil {
		return Component{}, fmt.Errorf("a component is a JSON object with type and id")
	}
	var c Component
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Component{}, fmt.Errorf("the component has a field of the wrong shape or an unknown one: %v", err)
	}
	if !slices.Contains(Types, c.Type) && !slices.Contains(TafTypes, c.Type) {
		return Component{}, fmt.Errorf("type must be one of %s", strings.Join(Types, ", "))
	}
	if !idPattern.MatchString(c.ID) {
		return Component{}, fmt.Errorf("id must be 1 to 64 of a-z 0-9 - _")
	}
	for k := range raw {
		if k != "type" && k != "id" && !slices.Contains(fields[c.Type], k) {
			return Component{}, fmt.Errorf("a %s has no field %q; it takes %s", c.Type, k, strings.Join(fields[c.Type], ", "))
		}
	}
	c.clean()
	if err := c.check(); err != nil {
		return Component{}, fmt.Errorf("%s %s: %w", c.Type, c.ID, err)
	}
	return c, nil
}

func text(name, s string, required bool, max int) error {
	switch n := utf8.RuneCountInString(s); {
	case required && strings.TrimSpace(s) == "":
		return fmt.Errorf("%s is required", name)
	case n > max:
		return fmt.Errorf("%s is %d characters, at most %d", name, n, max)
	}
	return nil
}

func (c Component) check() error {
	for _, l := range []struct{ name, s string }{{"yes", c.Yes}, {"no", c.No}} {
		if err := text(l.name, l.s, false, 40); err != nil {
			return err
		}
	}
	if err := text("note", c.Note, false, MaxLabel); err != nil {
		return err
	}
	switch c.Type {
	case "steps":
		if err := text("title", c.Title, false, MaxLabel); err != nil {
			return err
		}
		if len(c.Steps) == 0 || len(c.Steps) > MaxSteps {
			return fmt.Errorf("steps has 1 to %d steps", MaxSteps)
		}
		for _, s := range c.Steps {
			if err := text("a step's label", s.Label, true, MaxLabel); err != nil {
				return err
			}
			if !slices.Contains(statuses, s.Status) {
				return fmt.Errorf("a step's status is one of %s", strings.Join(statuses, ", "))
			}
		}
	case "drive":
		// The request the owner approves, in their words: the prompt names it (kiff/tafs#544).
		return text("text", c.Text, true, MaxLabel)
	case AskCoach:
		// The owner's request for Coach, in their words: the app writes it into Coach's box.
		return text("text", c.Text, true, MaxAsk)
	case "notice":
		if !slices.Contains(tones, c.Tone) {
			return fmt.Errorf("tone is one of %s", strings.Join(tones, ", "))
		}
		if err := text("title", c.Title, false, MaxLabel); err != nil {
			return err
		}
		return text("text", c.Text, true, MaxText)
	case "confirm":
		if err := text("text", c.Text, true, MaxText); err != nil {
			return err
		}
		if len(c.Options) < 2 || len(c.Options) > MaxOptions {
			return fmt.Errorf("options has 2 to %d options", MaxOptions)
		}
		if err := checkOptions(c.Options, false); err != nil {
			return err
		}
	case "questions":
		if err := text("text", c.Text, true, MaxText); err != nil {
			return err
		}
		if len(c.Questions) == 0 || len(c.Questions) > MaxQuestions {
			return fmt.Errorf("questions has 1 to %d questions", MaxQuestions)
		}
		seen := map[string]bool{}
		for i, q := range c.Questions {
			if !idPattern.MatchString(q.ID) {
				return fmt.Errorf("question %d id %q: use 1 to 64 of a-z 0-9 - _", i+1, q.ID)
			}
			if seen[q.ID] {
				return fmt.Errorf("question %d id %q: another question has it; each id is different", i+1, q.ID)
			}
			seen[q.ID] = true
			if err := text("a question's text", q.Text, true, MaxLabel); err != nil {
				return err
			}
			if err := text("a question's note", q.Note, false, MaxLabel); err != nil {
				return err
			}
			if len(q.Options) < 2 || len(q.Options) > MaxQuestionOptions {
				return fmt.Errorf("question %s has 2 to %d options", q.ID, MaxQuestionOptions)
			}
			if err := checkOptions(q.Options, true); err != nil {
				return fmt.Errorf("question %s: %w", q.ID, err)
			}
		}
	case "sign_in":
		if err := text("title", c.Title, false, MaxLabel); err != nil {
			return err
		}
		if err := text("text", c.Text, false, MaxText); err != nil {
			return err
		}
		if !refPattern.MatchString(c.Grant) {
			return fmt.Errorf("grant is planque's grant id")
		}
		if !refPattern.MatchString(c.Service) {
			return fmt.Errorf("service is the vendor's host: its MCP server's, or its API's base")
		}
	case "approval":
		if !refPattern.MatchString(c.Grant) {
			return fmt.Errorf("grant is planque's grant id")
		}
		if c.Draft != "" && !refPattern.MatchString(c.Draft) {
			return fmt.Errorf("draft is KIFF's agent draft id")
		}
	case "knob":
		if !refPattern.MatchString(c.TafID) {
			return fmt.Errorf("taf_id is the taf's tafs id")
		}
		if c.Version < 1 {
			return fmt.Errorf("version is the dial version taf_dial made")
		}
	case "result":
		if c.Taf == nil || !refPattern.MatchString(c.Taf.Agent) {
			return fmt.Errorf("taf.agent is the taf's agent id")
		}
		if err := text("taf.name", c.Taf.Name, true, MaxLabel); err != nil {
			return err
		}
		if c.Taf.Improves == "" {
			if !refPattern.MatchString(c.Taf.Draft) {
				return fmt.Errorf("taf.draft is the taf's KIFF draft id, the one the owner confirmed")
			}
			if c.Taf.Note != "" || c.Taf.Notes != "" {
				return fmt.Errorf("taf.note and taf.notes are for an improvement (taf.improves)")
			}
			// A new taf carries the job drafted with the owner, never one
			// of a fixed list (kiff/tafs#476).
			if c.Taf.Job != "" && c.Taf.Job != "custom" {
				return fmt.Errorf("a new taf carries the job you drafted with the owner (taf.kickoff, taf.instructions, taf.cron or taf.unscheduled); leave taf.job out")
			}
			if err := text("taf.kickoff", c.Taf.Kickoff, true, MaxJob); err != nil {
				return err
			}
			if err := text("taf.instructions", c.Taf.Instructions, false, MaxJob); err != nil {
				return err
			}
			if err := checkDisclose(c.Taf.Disclose); err != nil {
				return err
			}
			if err := writesFor(c.Taf.WritesFor); err != nil {
				return err
			}
			switch {
			case c.Taf.Unscheduled && c.Taf.Cron != "":
				return fmt.Errorf("taf.cron is for a taf with a schedule; an unscheduled one has none")
			case !c.Taf.Unscheduled && !cronPattern.MatchString(c.Taf.Cron):
				return fmt.Errorf("taf.cron is when it runs, five POSIX cron fields such as \"0 9 * * 1-5\", or set taf.unscheduled")
			}
		} else {
			if !idPattern.MatchString(c.Taf.Job) {
				return fmt.Errorf("taf.job is the taf's job, as tafs has it")
			}
			if c.Taf.Kickoff != "" || c.Taf.Instructions != "" || c.Taf.Cron != "" {
				return fmt.Errorf("taf.kickoff, taf.instructions and taf.cron are for a new taf; an improvement changes its notes")
			}
			if c.Taf.Unscheduled {
				return fmt.Errorf("taf.unscheduled is for a new taf: an improvement keeps its schedule")
			}
			// An improvement may add a disclosure, for a tool it adds or one a taf
			// from before never had; tafs adds it to the taf's, never taking one
			// away (agentgabo on #354).
			if err := checkDisclose(c.Taf.Disclose); err != nil {
				return err
			}
			if err := writesFor(c.Taf.WritesFor); err != nil {
				return err
			}
			if !refPattern.MatchString(c.Taf.Improves) {
				return fmt.Errorf("taf.improves is the tafs id of the taf it improves")
			}
			if c.Taf.Draft != "" && !refPattern.MatchString(c.Taf.Draft) {
				return fmt.Errorf("taf.draft is the KIFF draft id revising the taf's Cards")
			}
			if err := text("taf.note", c.Taf.Note, true, MaxText); err != nil {
				return err
			}
			if err := text("taf.notes", c.Taf.Notes, false, MaxNotes); err != nil {
				return err
			}
		}
		if err := text("taf.summary", c.Taf.Summary, false, MaxLabel); err != nil {
			return err
		}
		if err := text("text", c.Text, true, MaxText); err != nil {
			return err
		}
		if len(c.Tools) > MaxTools {
			return fmt.Errorf("tools names at most %d tools", MaxTools)
		}
		for _, t := range c.Tools {
			if !refPattern.MatchString(t) {
				return fmt.Errorf("tools are tool names")
			}
		}
	}
	return nil
}

// checkOptions checks a confirm's or a question's options: values unique,
// labels short, and a description only in a question.
func checkOptions(options []Option, described bool) error {
	seen := map[string]bool{}
	for i, o := range options {
		// Name the option and the value as sent, so the agent fixes it in
		// one turn. The value is never lower-cased here: it is what the
		// owner's answer carries back.
		if !idPattern.MatchString(o.Value) {
			return fmt.Errorf("option %d value %q: use 1 to 64 of a-z 0-9 - _", i+1, o.Value)
		}
		if seen[o.Value] {
			return fmt.Errorf("option %d value %q: another option has it; each value is different", i+1, o.Value)
		}
		seen[o.Value] = true
		if err := text("an option's label", o.Label, true, 80); err != nil {
			return err
		}
		if o.Description != "" && !described {
			return fmt.Errorf("a confirm's options take no description; use questions")
		}
		if err := text("an option's description", o.Description, false, MaxLabel); err != nil {
			return err
		}
	}
	return nil
}

// stepsDone refuses a step marked done whose named call this run made and
// that didn't go through: refused or held by KIFF, not made, failed, or of
// unknown effect. failed and skipped stay allowed, and a step that names no
// call, or a call this run didn't make, is as the model says.
func stepsDone(c Component, outcomes tool.Outcomes) error {
	for _, s := range c.Steps {
		if s.Status != "done" || s.Call == "" {
			continue
		}
		oc := outcomes(s.Call)
		if oc == "" || oc == "done" || strings.HasSuffix(oc, ": done") {
			continue
		}
		return fmt.Errorf("steps %s: %q can't be done: its call %s was %s. Mark it failed or skipped, or make the call again and show the plan once it went through", c.ID, s.Label, s.Call, oc)
	}
	return nil
}
