package memory_test

import (
	"testing"

	"github.com/kiff/agents/schedule"
	"github.com/kiff/agents/schedule/scheduletest"
	"github.com/kiff/agents/store/memory"
)

func TestSchedules(t *testing.T) {
	scheduletest.Run(t, func(*testing.T) schedule.Schedules { return memory.NewSchedules() })
}

func TestWaitings(t *testing.T) {
	scheduletest.RunWaitings(t, func(*testing.T) schedule.Waitings { return memory.NewWaitings() })
}
