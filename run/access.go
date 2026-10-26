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

// ReadGrant is one read of another person's work the account allowed and
// recorded first: who reads (Reader) whose sessions (Owner) with the agent,
// one session or, with none, the list of them, and why.
type ReadGrant struct {
	// Read is the account's record of this read, so a read served can be
	// matched to it.
	Read    string
	Reader  string
	Owner   string
	Session string
	Purpose string
}

// ReadGrantFunc asks the account what a read grant for the agent allows.
// An error is no read.
type ReadGrantFunc func(ctx context.Context, tenant, agent, grant string) (ReadGrant, error)

// ErrNoOversight is a read of another person's work the account didn't
// allow: no read grant, or one for something else.
var ErrNoOversight = errors.New("run: this read of another person's work isn't allowed")
