package runner

import (
	"context"
	"fmt"
	"strings"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/skill"
	"github.com/kiff/agents/tool"
)

// limit narrows a run's agent to its session's limit (session.Only): only the
// tools it names, and with Only.Skills, the session's own skills alone, so
// read_skill lists and reads nothing else and the agent's catalog of the rest
// leaves its instructions. A session without a limit is left as built.
func (x *execution) limit(ctx context.Context, a *agent.Agent, d registry.Definition, sess session.Session) error {
	only := sess.Only
	if only == nil {
		return nil
	}
	var own tool.Tool
	if only.Skills {
		if refs := d.CatalogSkills(); len(refs) > 0 {
			all, err := registry.LoadSkills(ctx, x.rn.Builder.Skills, x.tenant, refs)
			if err != nil {
				return err
			}
			// Instructions that map their skills were never given the catalog
			// (registry.Build, skill.Mapped): there's nothing to take out.
			// Otherwise fail closed: the catalog must be found to be taken
			// out, or the session would still list the other skills.
			if cat := skill.Catalog(all); cat != "" && !skill.Mapped(d.Instructions, all) {
				if !strings.Contains(a.System, "\n\n"+cat) {
					return fmt.Errorf("limited session: the skills catalog wasn't found in the instructions, so it can't be left out")
				}
				a.System = strings.Replace(a.System, "\n\n"+cat, "", 1)
			}
		}
		if refs := d.SessionRefs(sess.Skills); len(refs) > 0 {
			ps, err := registry.LoadSkills(ctx, x.rn.Builder.Skills, x.tenant, refs)
			if err != nil {
				return err
			}
			own = skillRead{Tool: skill.NewTool(ps), took: x.tookSkill}
		}
	}
	tools := make([]tool.Tool, 0, len(a.Tools))
	for _, t := range a.Tools {
		name := t.Spec().Name
		switch {
		case only.Skills && name == skill.ToolName:
			// The session's own skills' tool in place of the agent's, added below.
		case only.Offers(name, skill.ToolName):
			tools = append(tools, t)
		}
	}
	if own != nil {
		tools = append(tools, own)
	}
	a.Tools = tools
	return nil
}
