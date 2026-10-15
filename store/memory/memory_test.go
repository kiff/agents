package memory_test

import (
	"testing"

	"github.com/kiff/agents/session/sessiontest"
	"github.com/kiff/agents/store/memory"
)

func TestStore(t *testing.T) {
	sessiontest.Run(t, func() sessiontest.Backend { return memory.New() })
}
