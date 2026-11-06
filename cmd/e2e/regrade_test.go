package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/eval"
	"github.com/kiff/agents/model"
)

// The two transcripts the taf failed in, as the bucket keeps them (agentgabo,
// 6 Nov), and each with the one-clause rewrites of its narrated look-ups.
const (
	morning        = "testdata/regrade/42176052141.md"
	morningRewrite = "testdata/regrade/42176052141-one-clause.md"
	night          = "testdata/regrade/42146041790.md"
	nightRewrite   = "testdata/regrade/42146041790-one-clause.md"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A saved transcript gives back the taf's answers, the owner's message beside
// each, and the check before the approved hold: never its verdict line, nor
// anything after it.
func TestTheSavedTranscriptsReadBack(t *testing.T) {
	for _, c := range []struct {
		file    string
		answers int
		check   string
	}{
		{morning, 9, "Check charge ch_e2e_checked_1106-0646: can 700 cents of it be refunded? Don't refund it yet."},
		{morningRewrite, 9, "Check charge ch_e2e_checked_1106-0646: can 700 cents of it be refunded? Don't refund it yet."},
		{night, 8, "Check charge ch_e2e_checked_1106-0151: can 700 cents of it be refunded? Don't refund it yet."},
		{nightRewrite, 8, "Check charge ch_e2e_checked_1106-0151: can 700 cents of it be refunded? Don't refund it yet."},
	} {
		lines := readTafLines(readFile(t, c.file) + "owner: after the end → taf: not an answer the judges read\n")
		if len(lines) != c.answers {
			t.Fatalf("%s: %d answers, want %d: %+v", c.file, len(lines), c.answers, lines)
		}
		for _, l := range lines {
			if strings.HasPrefix(l.said, "not an answer") || strings.Contains(l.owner, "taf  ") {
				t.Fatalf("%s: read past the taf's answers: %+v", c.file, l)
			}
		}
		check, _, ask, text, ok := approvedAfterCheck(lines)
		if !ok || check != c.check || ask != "Refund it." || text == "" {
			t.Fatalf("%s: the approved hold after the check: %q / %q / %q (%v)", c.file, check, ask, text, ok)
		}
		if !strings.HasSuffix(lines[2].owner, heldApproved) {
			t.Fatalf("%s: answer 3's hold: %q", c.file, lines[2].owner)
		}
	}
	// The failing sentences are the bucket's, word for word.
	if s := readFile(t, morning); !strings.Contains(s, "Antes lo revisé: el cobro era de €1.000 y no tenía reembolsos previos.") ||
		!strings.Contains(s, "I read the charge first. It is €1,000 with nothing refunded yet.") {
		t.Fatal("the morning's transcript isn't the bucket's")
	}
}

// What -regrade reads back is what the run's judges read: answers recorded
// through answered, read from the transcript, give the same TafConcise text,
// whole, and AfterApproval the same chat but for the line breaks the
// transcript folds.
func TestARegradeReadsWhatTheRunJudged(t *testing.T) {
	var transcript, log strings.Builder
	tt := &test{cfg: config{transcript: &transcript, log: &log}, out: &redacting{w: &log}}
	long := strings.Repeat("palabra ", 60) + "fin."
	tt.answered("Check charge ch_e2e_checked_x: can 700 cents of it be refunded? Don't refund it yet.", "Yes, €7 can be refunded.\nThe charge is €1,000.")
	tt.answered("Refund it."+heldApproved, "You approved the €7 refund on that charge, and it went through once.")
	tt.answered("How many days?", long)
	if err := tt.tafConcise(context.Background()); err != nil {
		t.Fatal(err)
	}
	lines := readTafLines(transcript.String() + "owner: Refund it. → taf: the improve case, after the end\n")
	asked, said := make([]string, len(lines)), make([]string, len(lines))
	for i, l := range lines {
		asked[i], said[i] = l.owner, l.said
	}
	if got, want := tafAnswers(asked, said), tafAnswers(tt.tafAsked, tt.tafSaid); got != want {
		t.Fatalf("read back:\n%s\nwant what the judge read:\n%s", got, want)
	}
	if !strings.Contains(transcript.String(), "fin.") || strings.Contains(transcript.String(), "…") {
		t.Fatalf("an answer was cut in the transcript: %q", transcript.String())
	}
	check, checked, ask, text, ok := approvedAfterCheck(lines)
	if !ok || afterApprovalChat(check, checked, ask, text) != afterApprovalChat(tt.tafAsked[0], flat(tt.tafSaid[0]), "Refund it.", tt.tafSaid[1]) {
		t.Fatalf("the after-approval chat read back: %q", afterApprovalChat(check, checked, ask, text))
	}
}

// -regrade asks the taf's two judges, with this head's rules, about the
// transcript's answers, and says what each found.
func TestRegradeAsksTheTafsJudges(t *testing.T) {
	v := &verdicts{fail: []string{"Is each answer concise?"}}
	r, err := regrade(context.Background(), &eval.Judge{Model: v, ModelID: "judge", Price: model.Price{Input: 1, Output: 1}}, readFile(t, morning))
	if err != nil {
		t.Fatal(err)
	}
	if r.answers != 9 || r.concise.Pass || r.afterApproval == nil || !r.afterApproval.Pass || r.pass() {
		t.Fatalf("regraded %+v", r)
	}
	var concise, after int
	for _, p := range v.asked {
		switch {
		case strings.Contains(p, TafConcise) && strings.Contains(p, "1. Owner: Check charge ch_e2e_in_1106-0646, then refund 300 cents of it.\nTaf: Reembolsé €3"):
			concise++
		case strings.Contains(p, AfterApproval) && strings.Contains(p, "\n(held, approved on the phone, sent once)\nTaf: You approved the €7.00 refund"):
			after++
		}
	}
	if concise != eval.Votes || after != eval.Votes {
		t.Fatalf("asked TafConcise %d and AfterApproval %d times, want %d each", concise, after, eval.Votes)
	}
	var out bytes.Buffer
	r.print(&out)
	if !strings.Contains(out.String(), "concise, over 9 answers: FAIL") || !strings.Contains(out.String(), "after the approved hold: pass") {
		t.Fatalf("printed %q", out.String())
	}
}

// The taf's rule, the record note and the three judges give a look-up one
// shape: at most one short clause of the sentence that says what was done,
// none in a held call's sentence, no sentence of its own; a check with nothing
// to do is answered by what it found (kiff/tafs#560, Eval 42176052141).
func TestTheRulesAgreeOnOneShape(t *testing.T) {
	for _, c := range []struct {
		name, text string
		says       []string
	}{
		{"TafConcise", TafConcise, []string{
			"a sentence of its own about reading or checking the item it acts on",
			"what a look-up found in the sentence of a call that waits for the owner's approval",
			"At most one short clause of the sentence that says what was done may say what a look-up found",
			"asked only for a check, with nothing to do",
		}},
		{"AfterApproval", AfterApproval, []string{"leaving the check out passes", "one short clause of the sentence that says the refund went through"}},
		{"AfterRefusal", AfterRefusal, []string{"leaving it out passes", "one short clause of the sentence that says the refund wasn't made"}},
		{"RecordNote", agent.RecordNote, []string{"Neither gets a sentence of its own", "one short clause of the sentence that says what was done"}},
	} {
		for _, s := range c.says {
			if !strings.Contains(c.text, s) {
				t.Errorf("%s doesn't say %q", c.name, s)
			}
		}
	}
	if strings.Contains(agent.RecordNote, "say it was checked earlier") {
		t.Error("the record note still asks for a sentence about the check")
	}
}

// -regrade's verdicts quote the chat: never in CI, and never without the key.
func TestRegradeNeverRunsInCI(t *testing.T) {
	env := func(k string) string { return map[string]string{"ANTHROPIC_API_KEY": "k", "CI": "true"}[k] }
	err := run(context.Background(), []string{"-regrade", morning}, &redacting{w: &bytes.Buffer{}, ci: true}, env)
	if err == nil || !strings.Contains(err.Error(), "not in CI") {
		t.Fatalf("in CI: %v", err)
	}
	err = run(context.Background(), []string{"-regrade", morning}, &redacting{w: &bytes.Buffer{}}, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("without the key: %v", err)
	}
}

// The re-grade itself, with the Eval's judge on the Claude API: both saved
// transcripts fail concise as they did, on their narrated look-ups, and their
// one-clause rewrites pass both judges. It costs a few cents, so it runs only
// when asked: E2E_REGRADE=1 with ANTHROPIC_API_KEY.
func TestRegradeWithTheJudges(t *testing.T) {
	if os.Getenv("E2E_REGRADE") != "1" || os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("E2E_REGRADE=1 and ANTHROPIC_API_KEY run the judges on the saved transcripts")
	}
	j, err := judgeFrom("claude-sonnet-5-5", "2000000,10000000")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		file    string
		concise bool
	}{
		{morning, false}, {morningRewrite, true}, {night, false}, {nightRewrite, true},
	} {
		r, err := regrade(context.Background(), j, readFile(t, c.file))
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		r.print(&out)
		t.Logf("%s:\n%s", filepath.Base(c.file), out.String())
		if r.concise.Pass != c.concise {
			t.Errorf("%s: concise passed %v, want %v", c.file, r.concise.Pass, c.concise)
		}
		if c.concise && (r.afterApproval == nil || !r.afterApproval.Pass) {
			t.Errorf("%s: the one-clause answer after the approval didn't pass", c.file)
		}
	}
}
