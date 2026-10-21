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

// Types are the components the app draws.
var Types = []string{"steps", "notice", "confirm", "questions", "approval", "sign_in", "result"}

var (
	idPattern  = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)
	refPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
	statuses   = []string{"pending", "active", "done", "failed", "skipped"}
	tones      = []string{"info", "success", "warning", "error"}
)

// Step is one line of a plan.
type Step struct {
	Label  string `json:"label"`
	Status string `json:"status"`
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
	Job   string `json:"job"` // one of tafs' jobs, such as refunds
	// Draft is the taf's KIFF draft: for a new taf, the one the owner
	// confirmed in the second approval; for an improvement, the revision of
	// its Cards, which the owner confirms only when they switch to the
	// version. An improvement that changes no Card has none.
	Draft string `json:"draft,omitempty"`
	// Improves is the tafs id of the taf this is the next version of.
	Improves string `json:"improves,omitempty"`
	// Note says what changed, in the owner's language (an improvement).
	Note string `json:"note,omitempty"`
	// Notes are the taf's instructions from the owner for the new version,
	// in full; empty keeps them as they are (an improvement).
	Notes string `json:"notes,omitempty"`
	// Unscheduled says a new taf's tools can't find its work by themselves,
	// so it gets no schedule: the owner hands it work in its chat
	// (kiff/tafs#138). An improvement keeps the taf's schedule.
	Unscheduled bool `json:"unscheduled,omitempty"`
}

// MaxNotes bounds an improvement's instructions from the owner.
const MaxNotes = 4000

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
}

// Tool shows components.
type Tool struct{}

// New returns the show tool.
func New() Tool { return Tool{} }

// Access: showing changes only the agent's own chat.
func (Tool) Access() tool.Access { return tool.Own }
func (Tool) Gateway() bool       { return false }

// Shows: the show tool's components go to the owner's chat.
func (Tool) Shows() bool { return true }

func (Tool) Spec() model.ToolSpec {
	return model.ToolSpec{
		Name: Name,
		Description: "Show the owner a component in the chat; the app draws it. Showing an id again replaces that component in place (to advance steps). " +
			"steps {title, steps:[{label, status: pending|active|done|failed|skipped}]}; notice {tone: info|success|warning|error, title?, text}; " +
			"confirm {text, options:[{value, label}], 2 to 4}: the owner's choice comes back as their next message, \"[confirm <id>] <value>\"; " +
			"questions {text, questions:[{id, text, options:[{value, label, description?}], 2 to 5, multi?, note?}], 1 to 4}: several questions the owner answers together, when none depends on another's answer; " +
			"the app adds a field for the owner's own words under each, and Chat about this; the answers come back as one message, " +
			"[questions <id>] then a JSON object from each question's id to {value}, {values} (multi) or {own} (the owner's own words); " +
			"approval {grant, draft?}: the app shows planque's grant and KIFF's draft as those services say, and the owner approves with Face ID; " +
			"sign_in {grant, service}: the owner signs in at the vendor again; result {taf:{agent, name, job, draft}, text, tools}: hand the owner over to the taf, with its job (one of tafs' jobs, such as refunds) and the KIFF draft the owner confirmed; " +
			"to improve a taf instead, result {taf:{agent, name, job, improves, note, notes?, draft?}, text, tools}: improves is the taf's tafs id, note what changed, notes its full new instructions from the owner (omit to keep them), " +
			"and draft the KIFF draft revising its Cards, which the owner confirms only when they switch to the new version, never in this chat. " +
			"The app keeps no words of its own on a card: a sign_in takes title, text, yes (the button that opens the vendor's page), no (e.g. Not now) and note; a result takes yes (the button that opens the taf). " +
			"Write them in the owner's language. An approval takes no words of yours: what it grants, and its buttons, are planque's, KIFF's and the app's. " +
			"A component grants nothing: after an approval, check the grant's status in planque before going on.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"type":{"type":"string","enum":["steps","notice","confirm","questions","approval","sign_in","result"]},` +
			`"id":{"type":"string","description":"1 to 64 of a-z 0-9 - _; the same id replaces the component"},` +
			`"title":{"type":"string"},"text":{"type":"string"},` +
			`"tone":{"type":"string","enum":["info","success","warning","error"]},` +
			`"steps":{"type":"array","items":{"type":"object","properties":{"label":{"type":"string"},"status":{"type":"string","enum":["pending","active","done","failed","skipped"]}},"required":["label","status"]}},` +
			`"options":{"type":"array","items":{"type":"object","properties":{"value":{"type":"string"},"label":{"type":"string"}},"required":["value","label"]}},` +
			`"questions":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"text":{"type":"string"},"multi":{"type":"boolean"},` +
			`"options":{"type":"array","items":{"type":"object","properties":{"value":{"type":"string"},"label":{"type":"string"},"description":{"type":"string"}},"required":["value","label"]}},` +
			`"note":{"type":"string"}},"required":["id","text","options"]}},` +
			`"grant":{"type":"string","description":"planque's grant id"},"draft":{"type":"string","description":"KIFF's agent draft id"},` +
			`"service":{"type":"string","description":"the vendor's host, e.g. mcp.stripe.com"},` +
			`"taf":{"type":"object","properties":{"agent":{"type":"string"},"name":{"type":"string"},` +
			`"job":{"type":"string","description":"one of tafs' jobs, such as refunds"},"draft":{"type":"string","description":"the taf's KIFF draft id: confirmed by the owner for a new taf; an improvement's Card revision, confirmed at the switch"},` +
			`"improves":{"type":"string","description":"the tafs id of the taf this is the next version of"},` +
			`"note":{"type":"string","description":"what changed, in the owner's language (an improvement)"},` +
			`"notes":{"type":"string","description":"the taf's full new instructions from the owner (an improvement); omit to keep them"},` +
			`"unscheduled":{"type":"boolean","description":"a new taf whose tools can't find its work by themselves: no schedule, the owner hands it work in its chat"}},` +
			`"required":["agent","name","job"]},` +
			`"tools":{"type":"array","items":{"type":"string"}},` +
			`"yes":{"type":"string","description":"the main button's label (sign_in, result)"},` +
			`"no":{"type":"string","description":"the other button's label (sign_in)"},` +
			`"note":{"type":"string","description":"the line under the buttons (sign_in)"}},` +
			`"required":["type","id"]}`),
	}
}

// Call checks the component and returns it to be recorded. A component that
// doesn't fit is an error result naming what to fix.
func (Tool) Call(_ context.Context, input json.RawMessage) (tool.Result, error) {
	c, err := Parse(input)
	if err != nil {
		return tool.Result{Text: err.Error(), IsError: true}, nil
	}
	b, _ := json.Marshal(c)
	return tool.Result{Text: "shown: " + c.Type + " " + c.ID, Component: b}, nil
}

// Parse checks one component against contract 1 and returns it.
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
	if !slices.Contains(Types, c.Type) {
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
		for _, q := range c.Questions {
			if !idPattern.MatchString(q.ID) || seen[q.ID] {
				return fmt.Errorf("each question's id is 1 to 64 of a-z 0-9 - _, and different")
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
			return fmt.Errorf("service is the vendor's host, such as mcp.stripe.com")
		}
	case "approval":
		if !refPattern.MatchString(c.Grant) {
			return fmt.Errorf("grant is planque's grant id")
		}
		if c.Draft != "" && !refPattern.MatchString(c.Draft) {
			return fmt.Errorf("draft is KIFF's agent draft id")
		}
	case "result":
		if c.Taf == nil || !refPattern.MatchString(c.Taf.Agent) {
			return fmt.Errorf("taf.agent is the taf's agent id")
		}
		if err := text("taf.name", c.Taf.Name, true, MaxLabel); err != nil {
			return err
		}
		if !idPattern.MatchString(c.Taf.Job) {
			return fmt.Errorf("taf.job is one of tafs' jobs, such as refunds")
		}
		if c.Taf.Improves == "" {
			if !refPattern.MatchString(c.Taf.Draft) {
				return fmt.Errorf("taf.draft is the taf's KIFF draft id, the one the owner confirmed")
			}
			if c.Taf.Note != "" || c.Taf.Notes != "" {
				return fmt.Errorf("taf.note and taf.notes are for an improvement (taf.improves)")
			}
		} else {
			if c.Taf.Unscheduled {
				return fmt.Errorf("taf.unscheduled is for a new taf: an improvement keeps its schedule")
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
	for _, o := range options {
		if !idPattern.MatchString(o.Value) || seen[o.Value] {
			return fmt.Errorf("each option's value is 1 to 64 of a-z 0-9 - _, and different")
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
