package memory_test

import (
	"testing"
	"time"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/run/runtest"
	"github.com/kiff/agents/store/memory"
)

func TestRuns(t *testing.T) {
	runtest.Run(t, func(_ *testing.T, now func() time.Time) run.Runs { return memory.NewRunsAt(now) })
}
