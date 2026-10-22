package memory_test

import (
	"testing"

	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/usage"
	"github.com/kiff/agents/usage/usagetest"
)

func TestUsage(t *testing.T) {
	usagetest.Run(t, func(*testing.T) usage.Store { return memory.NewUsage() })
}
