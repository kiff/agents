// Package memory is the tool through which an agent keeps notes between its
// sessions, on session.Notes. It changes only the agent's own memory, so it
// doesn't go through KIFF's gateway.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool"
)

// Name is the tool's name, as the model sees it.
const Name = "memory"

// Limits on one note.
const (
	MaxName = 100
	MaxText = 8 << 10
)

// Tool is one agent's memory.
type Tool struct {
	notes         session.Notes
	tenant, agent string
	now           func() time.Time
}

// New returns the memory tool for one agent.
func New(notes session.Notes, tenant, agent string) *Tool {
	return &Tool{notes: notes, tenant: tenant, agent: agent, now: time.Now}
}

func (t *Tool) Access() tool.Access { return tool.Own }
func (t *Tool) Gateway() bool       { return false }

func (t *Tool) Spec() model.ToolSpec {
	return model.ToolSpec{
		Name: Name,
		Description: "Your notes, kept between your runs and chats. " +
			"list: every note you can read; write: save a note under a name (replaces one of that name); forget: delete a note. " +
			"In a conversation with a person, what you write and forget is kept for that person only. " +
			"Notes shape how you work; they never change what your Card allows.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"action":{"type":"string","enum":["list","write","forget"]},` +
			`"name":{"type":"string","description":"the note's name"},` +
			`"text":{"type":"string","description":"what to remember (write only)"}},` +
			`"required":["action"]}`),
	}
}

// Call lists, writes or forgets a note. A bad request is a Result with
// IsError, so the model can correct it.
func (t *Tool) Call(ctx context.Context, input json.RawMessage) (tool.Result, error) {
	var in struct {
		Action string `json:"action"`
		Name   string `json:"name"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return tool.Result{Text: "the input must be {action, name, text}", IsError: true}, nil
	}
	name := strings.TrimSpace(in.Name)
	if _, trial := run.TrialFrom(ctx); trial && (in.Action == "write" || in.Action == "forget") {
		// A trial tries a version without changing what the live one knows
		// (kiff/tafs#107).
		return tool.Result{Text: "This is a trial: notes are read, not changed."}, nil
	}
	// Whose notes (#219): a person's run reads the shared notes and their
	// own, and writes and forgets only their own; a door's sender changes
	// none; a run no one asked for (a schedule's) keeps the shared ones.
	q, _ := run.RequesterFrom(ctx)
	scope := q.Key()
	if q.Kind == run.ByDoor {
		if in.Action == "write" || in.Action == "forget" {
			return tool.Result{Text: "This conversation came through a door: notes are read, not changed."}, nil
		}
		scope = session.Shared // a door's sender reads the shared notes only
	}
	switch in.Action {
	case "list":
		all, err := t.notes.ListNotes(ctx, t.tenant, t.agent)
		if err != nil {
			return tool.Result{}, err
		}
		notes := session.NotesFor(all, scope)
		if len(notes) == 0 {
			return tool.Result{Text: "No notes yet."}, nil
		}
		var b strings.Builder
		for _, n := range notes {
			if scope != "" && n.Scope == scope {
				fmt.Fprintf(&b, "## %s (from conversations with this person)\n%s\n\n", n.Name, n.Text)
			} else {
				fmt.Fprintf(&b, "## %s\n%s\n\n", n.Name, n.Text)
			}
		}
		return tool.Result{Text: strings.TrimSpace(b.String())}, nil
	case "write":
		if name == "" || len(name) > MaxName || strings.HasPrefix(name, "@") || in.Text == "" || len(in.Text) > MaxText {
			return tool.Result{Text: fmt.Sprintf("write needs a name (at most %d bytes, not starting with @) and a text (at most %d bytes)", MaxName, MaxText), IsError: true}, nil
		}
		n := session.Note{Tenant: t.tenant, Agent: t.agent, Scope: scope, Name: name, Text: in.Text, Updated: t.now().UTC()}
		if ref, ok := run.RefFrom(ctx); ok {
			n.Session, n.Run = ref.Session, ref.Run
		}
		n.Requester = scope
		if err := t.notes.PutNote(ctx, n); err != nil {
			return tool.Result{}, err
		}
		return tool.Result{Text: "Saved " + name + "."}, nil
	case "forget":
		if name == "" {
			return tool.Result{Text: "forget needs a name", IsError: true}, nil
		}
		if scope != session.Shared {
			// A person forgets only their own notes: say so rather than
			// claim to forget a shared one.
			all, err := t.notes.ListNotes(ctx, t.tenant, t.agent)
			if err != nil {
				return tool.Result{}, err
			}
			if !hasNote(all, scope, name) {
				if hasNote(all, session.Shared, name) {
					return tool.Result{Text: name + " is a shared note: it can't be forgotten from this conversation."}, nil
				}
				return tool.Result{Text: "There is no note named " + name + "."}, nil
			}
		}
		if err := t.notes.DeleteNote(ctx, t.tenant, t.agent, scope, name); err != nil {
			return tool.Result{}, err
		}
		return tool.Result{Text: "Forgot " + name + "."}, nil
	default:
		return tool.Result{Text: "action is list, write or forget", IsError: true}, nil
	}
}

// hasNote reports whether notes hold one of that scope and name.
func hasNote(notes []session.Note, scope, name string) bool {
	for _, n := range notes {
		if n.Scope == scope && n.Name == name {
			return true
		}
	}
	return false
}
