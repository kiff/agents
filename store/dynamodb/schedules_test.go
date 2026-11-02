package dynamodb_test

import (
	"testing"

	"github.com/kiff/agents/schedule"
	"github.com/kiff/agents/schedule/scheduletest"
	"github.com/kiff/agents/store/dynamodb"
)

func TestSchedules(t *testing.T) {
	db := local(t)
	scheduletest.Run(t, func(t *testing.T) schedule.Schedules { return dynamodb.NewSchedules(db, table(t, db)) })
}

func TestWaitings(t *testing.T) {
	db := local(t)
	scheduletest.RunWaitings(t, func(t *testing.T) schedule.Waitings { return dynamodb.NewWaitings(db, table(t, db)) })
}
