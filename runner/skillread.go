package runner

import (
	"context"
	"encoding/json"

	"github.com/kiff/agents/skill"
	"github.com/kiff/agents/tool"
)

// watchSkillReads has the agent's read_skill note the skill each read
// took up (execution.tookSkill), for Run.FirstSkill. The tool itself is
// unchanged: same spec, same answers, nothing added to what the model reads.
func (x *execution) watchSkillReads(tools []tool.Tool) []tool.Tool {
	out := make([]tool.Tool, len(tools))
	for i, t := range tools {
		if t.Spec().Name == skill.ToolName {
			out[i] = skillRead{Tool: t, took: x.tookSkill}
		} else {
			out[i] = t
		}
	}
	return out
}

type skillRead struct {
	tool.Tool
	took func(string)
}

// Call reads as the skill tool does; a read that found the skill notes its
// name. One that didn't (a name it doesn't have, a bad input) notes nothing.
func (t skillRead) Call(ctx context.Context, input json.RawMessage) (tool.Result, error) {
	res, err := t.Tool.Call(ctx, input)
	if err != nil || res.IsError {
		return res, err
	}
	var in struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(input, &in) == nil {
		t.took(in.Name)
	}
	return res, nil
}
