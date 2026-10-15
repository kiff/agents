package session

import (
	"context"
	"time"
)

// Note is one thing an agent remembers between sessions, by name. The owner
// can read and forget each one.
type Note struct {
	Tenant  string    `json:"tenant"`
	Agent   string    `json:"agent"`
	Name    string    `json:"name"`
	Text    string    `json:"text"`
	Updated time.Time `json:"updated"`
}

// Notes keeps agents' memory. Its methods are named apart from Store's, so one
// backend can be both.
type Notes interface {
	// ListNotes returns an agent's notes, by name.
	ListNotes(ctx context.Context, tenant, agent string) ([]Note, error)
	// PutNote writes a note, replacing one of the same name.
	PutNote(ctx context.Context, n Note) error
	// DeleteNote forgets one note; forgetting a missing note is not an error.
	DeleteNote(ctx context.Context, tenant, agent, name string) error
	// DeleteNotes forgets all of an agent's notes.
	DeleteNotes(ctx context.Context, tenant, agent string) error
}

// Remove deletes everything kept for an agent: its sessions and its memory.
// Call it when the agent is removed for good.
func Remove(ctx context.Context, store Store, notes Notes, tenant, agent string) error {
	if err := store.DeleteAgent(ctx, tenant, agent); err != nil {
		return err
	}
	return notes.DeleteNotes(ctx, tenant, agent)
}
