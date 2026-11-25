package memory_test

import (
	"testing"

	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/usage"
	"github.com/kiff/agents/usage/usagetest"
)

func TestDays(t *testing.T) {
	usagetest.RunDays(t, func(*testing.T) usage.Days { return memory.NewDays() })
}
