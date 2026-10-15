package dynamodb_test

import (
	"testing"

	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/registry/registrytest"
	"github.com/kiff/agents/store/dynamodb"
)

func TestAgents(t *testing.T) {
	db := local(t)
	registrytest.Run(t, func(t *testing.T) registry.Agents { return dynamodb.NewAgents(db, table(t, db)) })
}
