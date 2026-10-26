package session

import (
	"context"
	"time"
)

// Note is one thing an agent remembers between sessions, by name. The owner
// can read and forget each one.
//
// A note is shared, read by every run of the agent, or one requester's
// (#219): what a person's conversation taught the agent, read only in that
// person's runs. A shared note whose origin isn't known (no run wrote it, no
// person's note became it, and no one shared it on purpose: notes from before
// scopes and provenance) is quarantined: no run reads it until someone who
// manages the agent shares it on purpose (SharedBy) or forgets it.
type Note struct {
	Tenant string `json:"tenant"`
	Agent  string `json:"agent"`
	// Scope is whose note it is: Shared, or a requester's Key
	// ("member:<subject>").
	Scope   string    `json:"scope,omitempty"`
	Name    string    `json:"name"`
	Text    string    `json:"text"`
	Updated time.Time `json:"updated"`
	// Session, Run and Requester are where the note was last written, and
	// whose conversation it came from; empty for one written outside a run,
	// by a run no one asked for, or before they were kept. Requester stays
	// when the note is shared.
	Session   string `json:"session,omitempty"`
	Run       string `json:"run,omitempty"`
	Requester string `json:"requester,omitempty"`
	// SharedBy is who made the note shared on purpose: the requester's key
	// of the person who shared it, or "owner" for one the owner wrote.
	SharedBy string `json:"shared_by,omitempty"`
}

// Quarantined reports whether n is a shared note of unknown origin, which no
// run reads.
func (n Note) Quarantined() bool {
	return n.Scope == Shared && n.Run == "" && n.Requester == "" && n.SharedBy == ""
}

// Shared is the scope of a note every run of the agent reads.
const Shared = ""

// NotesFor is the notes a run for owner reads (#219): the shared ones and
// owner's own, never another requester's. An empty owner reads the shared
// ones only. A quarantined note is read by no one.
func NotesFor(notes []Note, owner string) []Note {
	var out []Note
	for _, n := range notes {
		if (n.Scope == Shared && !n.Quarantined()) || (owner != "" && n.Scope == owner) {
			out = append(out, n)
		}
	}
	return out
}

// Notes keeps agents' memory. Its methods are named apart from Store's, so one
// backend can be both.
type Notes interface {
	// ListNotes returns all of an agent's notes, every scope, by scope then
	// name.
	ListNotes(ctx context.Context, tenant, agent string) ([]Note, error)
	// PutNote writes a note, replacing one of the same scope and name.
	PutNote(ctx context.Context, n Note) error
	// DeleteNote forgets one note of a scope; forgetting a missing note is
	// not an error.
	DeleteNote(ctx context.Context, tenant, agent, scope, name string) error
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
