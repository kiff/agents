package dynamodb_test

import (
	"testing"

	"github.com/kiff/agents/store/dynamodb"
	"github.com/kiff/agents/usage"
	"github.com/kiff/agents/usage/usagetest"
)

func TestUsage(t *testing.T) {
	db := local(t)
	usagetest.Run(t, func(t *testing.T) usage.Store { return dynamodb.NewUsage(db, table(t, db)) })
}
