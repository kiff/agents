package memory_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/store/memory"
	toolmemory "github.com/kiff/agents/tool/memory"
)

// kiff/tafs#107: a trial reads the taf's notes but changes none.
func TestATrialReadsNotesAndChangesNone(t *testing.T) {
	ctx := context.Background()
	notes := memory.New()
	m := toolmemory.New(notes, "t1", "rae")
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
	m := toolmemory.New(notes, "t1", "rae")
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
