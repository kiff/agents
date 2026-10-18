package memory_test

import (
	"testing"

	"github.com/kiff/agents/skill"
	"github.com/kiff/agents/skill/skilltest"
	"github.com/kiff/agents/store/memory"
)

func TestSkills(t *testing.T) {
	skilltest.Run(t, func(*testing.T) skill.Store { return memory.NewSkills() })
}
