package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/tool"
)

// ToolName is the skill tool's name, as the model sees it.
const ToolName = "skill"

// Tool reads the agent's skills: a skill's body, or one of its files. It
// reads only the packages the definition names, and changes nothing.
type Tool struct {
	byName map[string]Package
	names  []string
}

// NewTool returns the skill tool over an agent's packages.
func NewTool(ps []Package) *Tool {
	t := &Tool{byName: map[string]Package{}}
	for _, p := range ps {
		t.byName[p.Name] = p
		t.names = append(t.names, p.Name)
	}
	slices.Sort(t.names)
	return t
}

func (t *Tool) Access() tool.Access { return tool.Read }
func (t *Tool) Gateway() bool       { return false }

func (t *Tool) Spec() model.ToolSpec {
	return model.ToolSpec{
		Name: ToolName,
		Description: "Reads one of your skills (listed under Skills in your instructions): its guidance, " +
			"or a file it points to. Read a skill before doing what its description covers.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"name":{"type":"string","enum":` + mustJSON(t.names) + `},` +
			`"file":{"type":"string","description":"a file the skill names, by its path; leave out for the skill itself"}},` +
			`"required":["name"]}`),
	}
}

func mustJSON(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// Call returns the skill's body, or one of its files. A name or file it
// doesn't have is a Result with IsError, so the model can correct it.
func (t *Tool) Call(_ context.Context, input json.RawMessage) (tool.Result, error) {
	var in struct {
		Name string `json:"name"`
		File string `json:"file"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return tool.Result{Text: "the input is {name, file}", IsError: true}, nil
	}
	p, ok := t.byName[strings.TrimSpace(in.Name)]
	if !ok {
		return tool.Result{Text: fmt.Sprintf("no skill %q; yours: %s", in.Name, strings.Join(t.names, ", ")), IsError: true}, nil
	}
	file := strings.TrimPrefix(strings.TrimSpace(in.File), "./")
	if file == "" || file == "SKILL.md" {
		return tool.Result{Text: p.Body}, nil
	}
	text, ok := p.Files[file]
	if !ok {
		paths := make([]string, 0, len(p.Files))
		for path := range p.Files {
			paths = append(paths, path)
		}
		slices.Sort(paths)
		return tool.Result{Text: fmt.Sprintf("%s has no file %q; its files: %s", p.Name, in.File, strings.Join(paths, ", ")), IsError: true}, nil
	}
	return tool.Result{Text: text}, nil
}
