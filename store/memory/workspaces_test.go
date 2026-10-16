package memory_test

import (
	"testing"

	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/workspace"
	"github.com/kiff/agents/workspace/workspacetest"
)

func TestWorkspaces(t *testing.T) {
	workspacetest.Run(t, func(*testing.T) workspace.Workspaces { return memory.NewWorkspaces() })
}
