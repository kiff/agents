package dynamodb_test

import (
	"testing"

	"github.com/kiff/agents/store/dynamodb"
	"github.com/kiff/agents/usage"
	"github.com/kiff/agents/usage/usagetest"
)

func TestDays(t *testing.T) {
	db := local(t)
	usagetest.RunDays(t, func(t *testing.T) usage.Days { return dynamodb.NewDays(db, table(t, db)) })
}
