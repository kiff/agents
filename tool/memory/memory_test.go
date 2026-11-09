package memory_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
	toolmemory "github.com/kiff/agents/tool/memory"
)

// kiff/tafs#107: a trial reads the taf's notes but changes none.
func TestATrialReadsNotesAndChangesNone(t *testing.T) {
	ctx := context.Background()
	notes := memory.New()
	m := toolmemory.New(notes, "t1", "rae", 0)
	if r, err := m.Call(ctx, json.RawMessage(`{"action":"write","name":"rule","text":"Return first."}`)); err != nil || r.IsError {
		t.Fatalf("write: %+v, %v", r, err)
	}
	trial := run.WithTrial(ctx, run.Trial{Draft: "drf_2"})
	for _, in := range []string{`{"action":"write","name":"rule","text":"Refund at once."}`, `{"action":"forget","name":"rule"}`} {
		if r, err := m.Call(trial, json.RawMessage(in)); err != nil || !strings.Contains(r.Text, "trial") {
			t.Fatalf("%s in a trial: %+v, %v", in, r, err)
		}
	}
	got, err := m.Call(trial, json.RawMessage(`{"action":"list"}`))
	if err != nil || !strings.Contains(got.Text, "Return first.") {
		t.Fatalf("list in a trial: %+v, %v; want the note unchanged", got, err)
	}
}

// #219: a person's chat reads the shared notes and their own, writes and
// forgets only their own; another person's are never read; a door's sender
// reads the shared ones only and changes none; a run no one asked for keeps
// the shared ones.
func TestEachPersonKeepsTheirOwnNotes(t *testing.T) {
	ctx := context.Background()
	notes := memory.New()
	m := toolmemory.New(notes, "t1", "rae", 0)
	call := func(ctx context.Context, in string) string {
		t.Helper()
		r, err := m.Call(ctx, json.RawMessage(in))
		if err != nil || r.IsError {
			t.Fatalf("%s: %+v, %v", in, r, err)
		}
		return r.Text
	}
	// A schedule's run, with no requester, writes a shared note.
	call(ctx, `{"action":"write","name":"policy","text":"Refunds up to 50."}`)

	ana := run.WithRef(run.WithRequester(ctx, run.Requester{Kind: run.ByMember, ID: "ana"}), run.Ref{Session: "s1", Run: "r1"})
	luis := run.WithRequester(ctx, run.Requester{Kind: run.ByMember, ID: "luis"})
	door := run.WithRequester(ctx, run.Requester{Kind: run.ByDoor, ID: "client@example.com"})

	call(ana, `{"action":"write","name":"tone","text":"Ana likes it short."}`)
	if got := call(ana, `{"action":"list"}`); !strings.Contains(got, "Refunds up to 50.") || !strings.Contains(got, "Ana likes it short.") {
		t.Fatalf("ana lists %q", got)
	}
	for who, c := range map[string]context.Context{"luis": luis, "the door": door, "no one": ctx} {
		if got := call(c, `{"action":"list"}`); strings.Contains(got, "Ana") || !strings.Contains(got, "Refunds up to 50.") {
			t.Fatalf("%s lists %q", who, got)
		}
	}
	all, _ := notes.ListNotes(ctx, "t1", "rae")
	for _, n := range all {
		if n.Name == "tone" && (n.Scope != "member:ana" || n.Session != "s1" || n.Run != "r1" || n.Requester != "member:ana") {
			t.Fatalf("ana's note = %+v", n)
		}
	}

	// Luis can't forget Ana's note, nor the shared one; told so.
	if got := call(luis, `{"action":"forget","name":"tone"}`); !strings.Contains(got, "no note") {
		t.Fatalf("luis forgets ana's: %q", got)
	}
	if got := call(luis, `{"action":"forget","name":"policy"}`); !strings.Contains(got, "shared") {
		t.Fatalf("luis forgets the shared one: %q", got)
	}
	for _, in := range []string{`{"action":"write","name":"x","text":"y"}`, `{"action":"forget","name":"policy"}`} {
		if got := call(door, in); !strings.Contains(got, "door") {
			t.Fatalf("%s through a door: %q", in, got)
		}
	}
	if r, _ := m.Call(ana, json.RawMessage(`{"action":"write","name":"@x","text":"y"}`)); !r.IsError {
		t.Fatal("a name starting with @ was taken")
	}
	call(ana, `{"action":"forget","name":"tone"}`)
	if all, _ := notes.ListNotes(ctx, "t1", "rae"); len(all) != 1 || all[0].Name != "policy" {
		t.Fatalf("left %+v", all)
	}
}

// list is an index: each note's name, whose it is, the version that wrote it,
// when, and its first line, never its whole text; read gives one in full. A
// note written now records the agent's version; one from before shows none.
func TestListIsAnIndexAndReadGivesANoteWhole(t *testing.T) {
	ctx := context.Background()
	notes := memory.New()
	old := toolmemory.New(notes, "t1", "rae", 0)
	m := toolmemory.New(notes, "t1", "rae", 3)
	call := func(tl *toolmemory.Tool, ctx context.Context, in string) tool.Result {
		t.Helper()
		r, err := tl.Call(ctx, json.RawMessage(in))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	call(old, ctx, `{"action":"write","name":"legacy","text":"From before versions."}`)
	call(m, ctx, `{"action":"write","name":"policy","text":"Refunds up to 50.\nNever to a new card."}`)
	got := call(m, ctx, `{"action":"list"}`).Text
	today := time.Now().UTC().Format("2006-01-02")
	want := "2 notes. read one by name to see it in full.\n" +
		"- legacy (no version, " + today + "): From before versions.\n" +
		"- policy (version 3, " + today + "): Refunds up to 50.…"
	if got != want {
		t.Fatalf("list\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(got, "Never to a new card.") {
		t.Fatal("list gave a note's whole text")
	}
	if r := call(m, ctx, `{"action":"read","name":"policy"}`); r.IsError || r.Text != "## policy (version 3, "+today+")\nRefunds up to 50.\nNever to a new card." {
		t.Fatalf("read = %+v", r)
	}
	if r := call(m, ctx, `{"action":"read","name":"nothing"}`); !r.IsError {
		t.Fatalf("read of no note = %+v", r)
	}
	if r := call(m, ctx, `{"action":"read"}`); !r.IsError {
		t.Fatalf("read with no name = %+v", r)
	}
	// A person's own note comes before a shared one of the same name, as list shows both.
	ana := run.WithRequester(ctx, run.Requester{Kind: run.ByMember, ID: "ana"})
	call(m, ana, `{"action":"write","name":"policy","text":"Ana wants it short."}`)
	if r := call(m, ana, `{"action":"read","name":"policy"}`); !strings.HasSuffix(r.Text, "\nAna wants it short.") || !strings.Contains(r.Text, "from conversations with this person") {
		t.Fatalf("ana reads %q", r.Text)
	}
	if r := call(m, ctx, `{"action":"read","name":"policy"}`); !strings.HasSuffix(r.Text, "\nNever to a new card.") {
		t.Fatalf("the schedule reads %q", r.Text)
	}
}

// However many notes there are, list never runs past ListBudget, and never
// leaves one out silently: it says how many it left out and how to list on,
// and listing on from there reaches every note exactly once.
func TestListNeverCutsANoteSilently(t *testing.T) {
	ctx := context.Background()
	notes := memory.New()
	m := toolmemory.New(notes, "t1", "rae", 2)
	const total = 400
	for i := 0; i < total; i++ {
		in := fmt.Sprintf(`{"action":"write","name":"note-%03d","text":%q}`, i, strings.Repeat("x", 200))
		if r, err := m.Call(ctx, json.RawMessage(in)); err != nil || r.IsError {
			t.Fatalf("write %d: %+v, %v", i, r, err)
		}
	}
	seen := map[string]int{}
	after := ""
	for page := 0; page < total; page++ {
		in := `{"action":"list"}`
		if after != "" {
			in = fmt.Sprintf(`{"action":"list","after":%q}`, after)
		}
		r, err := m.Call(ctx, json.RawMessage(in))
		if err != nil || r.IsError {
			t.Fatalf("list: %+v, %v", r, err)
		}
		if len(r.Text) > toolmemory.ListBudget+512 {
			t.Fatalf("a list of %d bytes", len(r.Text))
		}
		if !strings.HasPrefix(r.Text, fmt.Sprintf("%d notes.", total)) {
			t.Fatalf("list head %q", strings.SplitN(r.Text, "\n", 2)[0])
		}
		next := ""
		for _, l := range strings.Split(r.Text, "\n")[1:] {
			switch {
			case strings.HasPrefix(l, "- "):
				seen[strings.SplitN(l[2:], " (", 2)[0]]++
			case strings.HasPrefix(l, "…and "):
				q := strings.Index(l, `after: "`)
				next = strings.TrimSuffix(l[q+len(`after: "`):], `".`)
			}
		}
		if next == "" {
			break
		}
		after = next
	}
	if len(seen) != total {
		t.Fatalf("listed %d of %d notes", len(seen), total)
	}
	for name, n := range seen {
		if n != 1 {
			t.Fatalf("%s listed %d times", name, n)
		}
	}
	if r, _ := m.Call(ctx, json.RawMessage(`{"action":"list","after":"zzz"}`)); !strings.Contains(r.Text, `No notes after "zzz".`) {
		t.Fatalf("list past the last = %q", r.Text)
	}
}

// listAll pages through list as a model would, following each "list with
// after", and counts how often each note's line is shown.
func listAll(t *testing.T, ctx context.Context, m tool.Tool) (map[string]int, []string) {
	t.Helper()
	seen := map[string]int{}
	var order []string
	after := ""
	for page := 0; page < 1000; page++ {
		in := `{"action":"list"}`
		if after != "" {
			in = fmt.Sprintf(`{"action":"list","after":%q}`, after)
		}
		r, err := m.Call(ctx, json.RawMessage(in))
		if err != nil || r.IsError {
			t.Fatalf("list: %+v, %v", r, err)
		}
		next := ""
		for _, l := range strings.Split(r.Text, "\n")[1:] {
			switch {
			case strings.HasPrefix(l, "- "):
				line := strings.SplitN(l[2:], ": ", 2)[0]
				seen[line]++
				order = append(order, line)
			case strings.HasPrefix(l, "…and "):
				q := strings.Index(l, `after: "`)
				next = strings.TrimSuffix(l[q+len(`after: "`):], `".`)
			}
		}
		if next == "" {
			return seen, order
		}
		after = next
	}
	t.Fatal("list never ended")
	return nil, nil
}

// A store sorts notes by scope, then name, but list pages by name: a
// person's own note whose name sorts before a page's cut is still listed,
// and a page never ends between their note and a shared one of that name.
func TestListPagesReachEveryNote(t *testing.T) {
	ctx := context.Background()
	notes := memory.New()
	shared := toolmemory.New(notes, "t1", "rae", 1)
	ana := run.WithRequester(ctx, run.Requester{Kind: run.ByMember, ID: "ana"})
	own := toolmemory.New(notes, "t1", "rae", 1)
	write := func(ctx context.Context, m tool.Tool, name, text string) {
		t.Helper()
		in := fmt.Sprintf(`{"action":"write","name":%q,"text":%q}`, name, text)
		if r, err := m.Call(ctx, json.RawMessage(in)); err != nil || r.IsError {
			t.Fatalf("write %s: %+v, %v", name, r, err)
		}
	}
	big := strings.Repeat("x", 150)
	for i := 1; i <= 60; i++ {
		for c := 'a'; c <= 'e'; c++ {
			write(ctx, shared, fmt.Sprintf("b%02d%c", i, c), big)
		}
	}
	write(ana, own, "a-own", "mine")
	// ana has a note of every shared note's name too, so some page's cut
	// falls between two notes of one name.
	for i := 1; i <= 60; i++ {
		for c := 'a'; c <= 'e'; c++ {
			write(ana, own, fmt.Sprintf("b%02d%c", i, c), big)
		}
	}
	seen, order := listAll(t, ana, own)
	total := 300 + 1 + 300
	// Her own note comes before the shared one of its name, as read finds them.
	if len(order) == 0 || !strings.HasPrefix(order[0], "a-own ") {
		t.Fatalf("the first line %q; want ana's a-own", order)
	}
	for i := 1; i+1 < len(order); i += 2 {
		mine, theirs := order[i], order[i+1]
		if strings.SplitN(mine, " ", 2)[0] != strings.SplitN(theirs, " ", 2)[0] ||
			!strings.Contains(mine, "from conversations with this person") || strings.Contains(theirs, "from conversations with this person") {
			t.Fatalf("lines %d and %d: %q, then %q; want her own note, then the shared one", i, i+1, mine, theirs)
		}
	}
	n := 0
	for line, k := range seen {
		if k != 1 {
			t.Fatalf("%q listed %d times", line, k)
		}
		n++
	}
	if n != total {
		t.Fatalf("the pages listed %d of %d notes", n, total)
	}
}
