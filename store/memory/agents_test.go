package memory_test

import (
	"testing"

	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/registry/registrytest"
	"github.com/kiff/agents/store/memory"
)

func TestAgents(t *testing.T) {
	registrytest.Run(t, func(*testing.T) registry.Agents { return memory.NewAgents() })
}
