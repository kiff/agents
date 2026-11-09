// Package memory is the tool through which an agent keeps notes between its
// sessions, on session.Notes. It changes only the agent's own memory, so it
// doesn't go through KIFF's gateway.
package memory

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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

// ListBudget bounds what one list answers, in bytes, well inside what one
// tool result may put in the model's context (agent.DefaultMaxToolResult): a
// list is never cut there. Past it, list says how many notes it left out and
// how to list on from where it stopped, so no note is ever silently missing.
const ListBudget = 16 << 10

// FirstLine bounds the first line list shows of each note, in bytes.
const FirstLine = 160

// Tool is one agent's memory.
type Tool struct {
	notes         session.Notes
	tenant, agent string
	version       int
	now           func() time.Time
}

// New returns the memory tool for one agent, at its version (0: none), which
// each note it writes records.
func New(notes session.Notes, tenant, agent string, version int) *Tool {
	return &Tool{notes: notes, tenant: tenant, agent: agent, version: version, now: time.Now}
}

func (t *Tool) Access() tool.Access { return tool.Own }
func (t *Tool) Gateway() bool       { return false }

func (t *Tool) Spec() model.ToolSpec {
	return model.ToolSpec{
		Name: Name,
		Description: "Your notes, kept between your runs and chats. " +
			"list: every note you can read, each by its name, the version that wrote it, when, and its first line (after: list on past that name); " +
			"read: one note in full, by name; write: save a note under a name (replaces one of that name); forget: delete a note. " +
			"In a conversation with a person, what you write and forget is kept for that person only. " +
			"Notes shape how you work; they never change what your Card allows.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"action":{"type":"string","enum":["list","read","write","forget"]},` +
			`"name":{"type":"string","description":"the note's name"},` +
			`"text":{"type":"string","description":"what to remember (write only)"},` +
			`"after":{"type":"string","description":"list only: the last name a list stopped at, to list on from there"}},` +
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
		After  string `json:"after"`
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
		return tool.Result{Text: index(notes, scope, strings.TrimSpace(in.After))}, nil
	case "read":
		if name == "" {
			return tool.Result{Text: "read needs a name", IsError: true}, nil
		}
		all, err := t.notes.ListNotes(ctx, t.tenant, t.agent)
		if err != nil {
			return tool.Result{}, err
		}
		// A person's own note before a shared one of the same name, as list shows both.
		notes := session.NotesFor(all, scope)
		for _, own := range []bool{true, false} {
			for _, n := range notes {
				if n.Name == name && (n.Scope == scope) == own {
					return tool.Result{Text: "## " + heading(n, scope) + "\n" + n.Text}, nil
				}
			}
		}
		return tool.Result{Text: "There is no note named " + name + ".", IsError: true}, nil
	case "write":
		if name == "" || len(name) > MaxName || strings.HasPrefix(name, "@") || in.Text == "" || len(in.Text) > MaxText {
			return tool.Result{Text: fmt.Sprintf("write needs a name (at most %d bytes, not starting with @) and a text (at most %d bytes)", MaxName, MaxText), IsError: true}, nil
		}
		n := session.Note{Tenant: t.tenant, Agent: t.agent, Scope: scope, Name: name, Text: in.Text, Updated: t.now().UTC(), Version: t.version}
		if ref, ok := run.RefFrom(ctx); ok {
			n.Session, n.Run = ref.Session, ref.Run
		} else if scope == session.Shared {
			n.SharedBy = "owner" // outside any run: the owner's own chat (cmd/chat)
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
		return tool.Result{Text: "action is list, read, write or forget", IsError: true}, nil
	}
}

// index is list's answer: how many notes there are, then one line for each,
// in order, past after: its name, whose it is, the version that wrote it,
// when, and its first line. It stops before ListBudget and says how many it
// left out and how to list on, so the answer is never cut.
func index(notes []session.Note, scope, after string) string {
	// By name, a person's own note before a shared one of the same name (as
	// read finds them), so after, a name, is a true cursor: a store sorts by
	// scope first, and paging that by name would pass some notes over.
	notes = slices.Clone(notes)
	slices.SortStableFunc(notes, func(a, b session.Note) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return cmp.Compare(boolInt(a.Scope == session.Shared), boolInt(b.Scope == session.Shared))
	})
	var lines []string
	shown, size := 0, 0
	skipped := 0
	for i, n := range notes {
		if after != "" && n.Name <= after {
			skipped++
			continue
		}
		line := "- " + heading(n, scope)
		if first := firstLine(n.Text); first != "" {
			line += ": " + first
		}
		// A page never ends between two notes of one name (a person's own
		// and a shared one): the next page starts after that name. Each line
		// is bounded, so the one over is small.
		if size+len(line)+1 > ListBudget && shown > 0 && n.Name != notes[i-1].Name {
			rest := len(notes) - i
			lines = append(lines, fmt.Sprintf("…and %d more. To see them, list with after: %q.", rest, notes[i-1].Name))
			break
		}
		lines = append(lines, line)
		shown++
		size += len(line) + 1
	}
	head := fmt.Sprintf("%d notes. read one by name to see it in full.", len(notes))
	if len(notes) == 1 {
		head = "1 note. read it by name to see it in full."
	}
	if skipped > 0 {
		head += fmt.Sprintf(" These come after %q.", after)
	}
	if shown == 0 {
		return head + "\nNo notes after " + strconv.Quote(after) + "."
	}
	return head + "\n" + strings.Join(lines, "\n")
}

// heading is a note's name and what list says of it: whose it is, the version
// that wrote it, and when.
func heading(n session.Note, scope string) string {
	var tags []string
	if scope != "" && n.Scope == scope {
		tags = append(tags, "from conversations with this person")
	}
	if n.Version > 0 {
		tags = append(tags, "version "+strconv.Itoa(n.Version))
	} else {
		tags = append(tags, "no version")
	}
	if !n.Updated.IsZero() {
		tags = append(tags, n.Updated.UTC().Format("2006-01-02"))
	}
	return n.Name + " (" + strings.Join(tags, ", ") + ")"
}

// firstLine is a note's first non-empty line, cut at FirstLine bytes on a
// rune boundary, with "…" when there's more of it.
func firstLine(text string) string {
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		more := strings.Contains(strings.TrimSpace(text), "\n") || len(l) > FirstLine
		if len(l) > FirstLine {
			cut := FirstLine
			for cut > 0 && !utf8.RuneStart(l[cut]) {
				cut--
			}
			l = l[:cut]
		}
		if more {
			l += "…"
		}
		return l
	}
	return ""
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

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
