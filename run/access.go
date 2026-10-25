package run

import (
	"context"
	"errors"
)

// Access is what a person may do with an agent, as the account answers it
// (#221): use it (start runs, read their own sessions), or oversee it (read
// everyone's work with it).
type Access struct {
	Use     bool
	Oversee bool
}

// AccessFunc answers what the person with this subject may do with the
// agent. An error is no answer, which is no access.
type AccessFunc func(ctx context.Context, tenant, agent, subject string) (Access, error)

// ErrNoAccess is a member's run, or read, of an agent they may not use.
var ErrNoAccess = errors.New("run: this person may not use this agent")
