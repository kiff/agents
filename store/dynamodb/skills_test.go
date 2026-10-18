package dynamodb_test

import (
	"testing"

	"github.com/kiff/agents/skill"
	"github.com/kiff/agents/skill/skilltest"
	"github.com/kiff/agents/store/dynamodb"
)

func TestSkills(t *testing.T) {
	db := local(t)
	skilltest.Run(t, func(t *testing.T) skill.Store { return dynamodb.NewSkills(db, table(t, db)) })
}
