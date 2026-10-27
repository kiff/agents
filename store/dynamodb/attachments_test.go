package dynamodb_test

import (
	"testing"

	"github.com/kiff/agents/attach"
	"github.com/kiff/agents/attach/attachtest"
	"github.com/kiff/agents/store/dynamodb"
)

func TestAttachments(t *testing.T) {
	db := local(t)
	attachtest.Run(t, func(t *testing.T) attach.Store { return dynamodb.NewAttachments(db, table(t, db)) })
}
