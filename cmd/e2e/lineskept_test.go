package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kiff/agents/eval"
)

// noticeRun is a setup run that showed a rules notice with lines, agreed to
// it, and made a taf with instructions, over a connection that pins pins.
func noticeRun(t *testing.T, lang, instructions string, pins ...string) said {
	t.Helper()
	cards := map[string]component{}
	for id, raw := range map[string]string{
		"rules": `{"type":"notice","id":"rules","tone":"info","text":"Adds at most 50 rows a day\\nWrites only to the Leads sheet (the connection keeps it)\\nSkips anyone it already wrote to (in its instructions)","lang":"` + lang + `","rules":[{"tool":"append_row","over":"refuse"}],"lines":[` +
			`{"text":"Adds at most 50 rows a day","kept":"card","tools":["append_row"]},` +
			`{"text":"Writes only to the Leads sheet","kept":"connection","tools":["append_row"]},` +
			`{"text":"Skips anyone it already wrote to","kept":"instructions"}]}`,
		"agree": `{"type":"confirm","id":"agree","agrees":"rules","options":[{"value":"agree","label":"Agree"},{"value":"change","label":"Change something"}]}`,
		"done":  `{"type":"result","id":"done","taf":{"agent":"a1","name":"Leads","instructions":` + jsonString(instructions) + `}}`,
	} {
		var c component
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		cards[id] = c
	}
	return said{cards: cards, order: []string{"rules", "agree", "done"}, pinned: pins, connected: []grantTool{{}}}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

// What a rules notice's lines claim is read as data before any judge votes:
// its language is the owner's, an instructions line is in the taf's
// instructions word for word, and a connection line's tool is pinned.
func TestAnAgreedNoticesLinesAreKeptWhereTheySay(t *testing.T) {
	const instr = "Add each new lead as a row.\nSkips anyone it already wrote to.\nNever delete a row."
	pin := "append_row_k3f9: spreadsheet = Leads"
	if why := unkeptLines(noticeRun(t, "en", instr, pin), "en"); why != "" {
		t.Fatalf("every line kept: %s", why)
	}
	for name, c := range map[string]struct {
		s    said
		lang string
		want string
	}{
		"the notice in another language": {noticeRun(t, "en", instr, pin), "fr", `is in "en", but the owner writes "fr"`},
		"an instructions line the taf wasn't given": {noticeRun(t, "en", "Add each new lead as a row.", pin), "en",
			`"Skips anyone it already wrote to" says it's in the taf's instructions`},
		"a connection line nothing pins": {noticeRun(t, "en", instr, "read_rows: spreadsheet = Leads"), "en",
			"no approved connection pins append_row"},
	} {
		if why := unkeptLines(c.s, c.lang); !strings.Contains(why, c.want) {
			t.Errorf("%s: %q, want %q", name, why, c.want)
		}
	}
	// No connection approved yet, or no taf made: those lines are left to the judge.
	s := noticeRun(t, "en", "", pin)
	s.connected = nil
	if why := unkeptLines(s, "en"); why != "" {
		t.Errorf("nothing connected or made: %s", why)
	}
	// A notice from before lines says nothing here.
	old := noticeRun(t, "en", instr)
	n := old.cards["rules"]
	n.Lines, n.Lang = nil, ""
	old.cards["rules"] = n
	if why := unkeptLines(old, "fr"); why != "" {
		t.Errorf("a text-only notice: %s", why)
	}
}

// signsWhatWasAgreed fails on an unkept line before the judge is asked: the
// judge here would pass anything.
func TestSignsWhatWasAgreedFailsOnAnUnkeptLineWithoutAVote(t *testing.T) {
	s := noticeRun(t, "en", "Add each new lead as a row.", "append_row_k3f9: spreadsheet = Leads")
	r, err := gradeAgreed(context.Background(), &eval.Judge{Model: &verdicts{}, ModelID: "judge"}, s, []map[string]any{{"id": "c1"}}, "en")
	if err != nil {
		t.Fatal(err)
	}
	if r.Grade.Pass || r.Grade.Skipped || !strings.Contains(r.Grade.Why, "says it's in the taf's instructions") {
		t.Fatalf("%+v", r.Grade)
	}
}
