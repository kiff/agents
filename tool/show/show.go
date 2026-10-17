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
)

// Types are the components the app draws.
var Types = []string{"steps", "notice", "confirm", "approval", "sign_in", "result"}

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

// Option is one answer to a confirm.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Taf is the taf a result hands over to.
type Taf struct {
	Agent string `json:"agent"`
	Name  string `json:"name"`
}

// Component is one component. Which fields it has depends on its type.
type Component struct {
	Type    string   `json:"type"`
	ID      string   `json:"id"`
	Title   string   `json:"title,omitempty"`
	Text    string   `json:"text,omitempty"`
	Tone    string   `json:"tone,omitempty"`
	Steps   []Step   `json:"steps,omitempty"`
	Options []Option `json:"options,omitempty"`
	Grant   string   `json:"grant,omitempty"`
	Draft   string   `json:"draft,omitempty"`
	Service string   `json:"service,omitempty"`
	Taf     *Taf     `json:"taf,omitempty"`
	Tools   []string `json:"tools,omitempty"`
}

// fields lists the fields each type may carry, besides type and id.
var fields = map[string][]string{
	"steps":    {"title", "steps"},
	"notice":   {"tone", "title", "text"},
	"confirm":  {"text", "options"},
	"approval": {"grant", "draft"},
	"sign_in":  {"grant", "service"},
	"result":   {"taf", "text", "tools"},
}

// Tool shows components.
type Tool struct{}

// New returns the show tool.
func New() Tool { return Tool{} }

// Access: showing changes only the agent's own chat.
func (Tool) Access() tool.Access { return tool.Own }
func (Tool) Gateway() bool       { return false }

func (Tool) Spec() model.ToolSpec {
	return model.ToolSpec{
		Name: Name,
		Description: "Show the owner a component in the chat; the app draws it. Showing an id again replaces that component in place (to advance steps). " +
			"steps {title, steps:[{label, status: pending|active|done|failed|skipped}]}; notice {tone: info|success|warning|error, title?, text}; " +
			"confirm {text, options:[{value, label}], 2 to 4}: the owner's choice comes back as their next message, \"[confirm <id>] <value>\"; " +
			"approval {grant, draft?}: the app shows planque's grant and KIFF's draft as those services say, and the owner approves with Face ID; " +
			"sign_in {grant, service}: the owner signs in at the vendor again; result {taf:{agent, name}, text, tools}: hand the owner over to the taf. " +
			"A component grants nothing: after an approval, check the grant's status in planque before going on.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"type":{"type":"string","enum":["steps","notice","confirm","approval","sign_in","result"]},` +
			`"id":{"type":"string","description":"1 to 64 of a-z 0-9 - _; the same id replaces the component"},` +
			`"title":{"type":"string"},"text":{"type":"string"},` +
			`"tone":{"type":"string","enum":["info","success","warning","error"]},` +
			`"steps":{"type":"array","items":{"type":"object","properties":{"label":{"type":"string"},"status":{"type":"string","enum":["pending","active","done","failed","skipped"]}},"required":["label","status"]}},` +
			`"options":{"type":"array","items":{"type":"object","properties":{"value":{"type":"string"},"label":{"type":"string"}},"required":["value","label"]}},` +
			`"grant":{"type":"string","description":"planque's grant id"},"draft":{"type":"string","description":"KIFF's agent draft id"},` +
			`"service":{"type":"string","description":"the vendor's host, e.g. mcp.stripe.com"},` +
			`"taf":{"type":"object","properties":{"agent":{"type":"string"},"name":{"type":"string"}},"required":["agent","name"]},` +
			`"tools":{"type":"array","items":{"type":"string"}}},` +
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
		seen := map[string]bool{}
		for _, o := range c.Options {
			if !idPattern.MatchString(o.Value) || seen[o.Value] {
				return fmt.Errorf("each option's value is 1 to 64 of a-z 0-9 - _, and different")
			}
			seen[o.Value] = true
			if err := text("an option's label", o.Label, true, 80); err != nil {
				return err
			}
		}
	case "approval":
		if !refPattern.MatchString(c.Grant) {
			return fmt.Errorf("grant is planque's grant id")
		}
		if c.Draft != "" && !refPattern.MatchString(c.Draft) {
			return fmt.Errorf("draft is KIFF's agent draft id")
		}
	case "sign_in":
		if !refPattern.MatchString(c.Grant) {
			return fmt.Errorf("grant is planque's grant id")
		}
		if !refPattern.MatchString(c.Service) {
			return fmt.Errorf("service is the vendor's host, such as mcp.stripe.com")
		}
	case "result":
		if c.Taf == nil || !refPattern.MatchString(c.Taf.Agent) {
			return fmt.Errorf("taf.agent is the taf's agent id")
		}
		if err := text("taf.name", c.Taf.Name, true, MaxLabel); err != nil {
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
