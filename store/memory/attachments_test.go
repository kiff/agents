package memory

import (
	"testing"

	"github.com/kiff/agents/attach"
	"github.com/kiff/agents/attach/attachtest"
)

func TestAttachments(t *testing.T) {
	attachtest.Run(t, func(*testing.T) attach.Store { return NewAttachments() })
}
