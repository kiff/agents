package main

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/eval"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
)

func called(run string, tool, input, outcome string) session.Event {
	b, _ := json.Marshal(agent.CalledBody{Tool: tool, Input: json.RawMessage(input), Outcome: outcome})
	return session.Event{Run: run, Kind: session.Called, Body: b}
}

// An answer's record is its chat's calls up to its run's: a look-up one
// message before is in it, marked earlier, and a later message's calls
// aren't. A held call answered on the phone is listed once, as it ended,
// though the run's store gives its input back with its keys reordered.
func TestAnAnswersRecordIsItsChatsCallsUpToItsRun(t *testing.T) {
	events := []session.Event{
		{Run: "check", Kind: session.OwnerMessage},
		called("check", "retrieve_charge_7f2a", `{"charge":"ch_1"}`, agent.OutcomeDone),
		{Run: "check", Kind: session.AgentMessage},
		{Run: "refund", Kind: session.OwnerMessage},
		called("refund", "create_refund_7f2a", `{"charge":"ch_1","amount":700}`, agent.OutcomeHeld),
		called("refund", "create_refund_7f2a", `{"amount": 700, "charge": "ch_1"}`, agent.OutcomeApproved),
		{Run: "refund", Kind: session.AgentMessage},
		{Run: "later", Kind: session.OwnerMessage},
		called("later", "create_refund_7f2a", `{"charge":"ch_2","amount":100}`, agent.OutcomeDone),
	}
	got, err := calledUpTo(events, "refund")
	if err != nil {
		t.Fatal(err)
	}
	want := []eval.RecordedCall{
		{Tool: "retrieve_charge_7f2a", Input: `{"charge":"ch_1"}`, Outcome: agent.OutcomeDone, Earlier: true},
		{Tool: "create_refund_7f2a", Input: `{"charge":"ch_1","amount":700}`, Outcome: agent.OutcomeApproved},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the refund's record:\n%+v\nwant\n%+v", got, want)
	}
	if text := eval.RecordedText(got, "x"); !strings.Contains(text, "1. (earlier) retrieve_charge_7f2a") || !strings.Contains(text, "2. create_refund_7f2a") {
		t.Fatalf("the judge reads:\n%s", text)
	}
	// The check's own record is its look-up alone, not marked earlier.
	if got, _ := calledUpTo(events, "check"); len(got) != 1 || got[0].Earlier || got[0].Outcome != agent.OutcomeDone {
		t.Fatalf("the check's record: %+v", got)
	}
	// A hold the owner hasn't answered stays held: "it waits for your approval" is read against it.
	if got, _ := calledUpTo(events[:5], "refund"); len(got) != 2 || got[1].Outcome != agent.OutcomeHeld {
		t.Fatalf("an unanswered hold's record: %+v", got)
	}
	// Another refund of the same tool, held after the first was answered, stays its own call.
	more := append(append([]session.Event(nil), events[:7]...), called("refund", "create_refund_7f2a", `{"charge":"ch_1","amount":50}`, agent.OutcomeHeld))
	if got, _ := calledUpTo(more, "refund"); len(got) != 3 || got[1].Outcome != agent.OutcomeApproved || got[2].Outcome != agent.OutcomeHeld {
		t.Fatalf("two refunds of one tool: %+v", got)
	}
	if _, err := calledUpTo(events, "elsewhere"); err == nil {
		t.Fatal("a run not in the chat was given a record")
	}
}

// seedRun stores run's events in chat, in order, from at.
func seedRun(t *testing.T, s *memory.Store, chat, run string, at time.Time, events ...session.Event) {
	t.Helper()
	ctx := context.Background()
	if err := s.Begin(ctx, "t1", chat, run, at); err != nil {
		t.Fatal(err)
	}
	for i := range events {
		events[i].Seq, events[i].Time = int64(i+1), at.Add(time.Duration(i)*time.Millisecond)
	}
	if err := s.Append(ctx, "t1", chat, run, events); err != nil {
		t.Fatal(err)
	}
}

// Every answer of the taf's is judged against its own record, by majority:
// one the record contradicts fails the run, named by its number, with the
// judges' reason in the transcript and never in CI's log.
func TestEachAnswerIsJudgedAgainstItsRecord(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	store := memory.New()
	for _, id := range []string{"chat", "file"} {
		if err := store.Create(ctx, session.Session{ID: id, Tenant: "t1", Agent: "taf", Started: now, Updated: now}); err != nil {
			t.Fatal(err)
		}
	}
	seedRun(t, store, "chat", "check", now, session.Event{Kind: session.OwnerMessage},
		called("", "retrieve_charge_7f2a", `{"charge":"ch_1"}`, agent.OutcomeDone))
	seedRun(t, store, "chat", "refund", now.Add(time.Second), session.Event{Kind: session.OwnerMessage},
		called("", "create_refund_7f2a", `{"charge":"ch_1","amount":700}`, agent.OutcomeHeld),
		called("", "create_refund_7f2a", `{"charge":"ch_1","amount":700}`, agent.OutcomeApproved))
	seedRun(t, store, "file", "policy", now.Add(2*time.Second), session.Event{Kind: session.OwnerMessage})

	judged := func(fail ...string) (*verdicts, string, string, error) {
		v := &verdicts{fail: fail}
		var log, transcript strings.Builder
		tt := &test{tenant: "t1", sessions: store, cfg: config{log: &log, transcript: &transcript, judge: &eval.Judge{Model: v, ModelID: "judge"}},
			out: &redacting{w: &log, ci: true}}
		tt.answered("chat", "check", "Check charge ch_1: can 700 cents of it be refunded? Don't refund it yet.", "Yes, €7 can be refunded. I haven't refunded anything.")
		tt.answered("chat", "refund", "Refund it."+heldApproved, "You approved the €7 refund, and it went through once. The charge look-up isn't in this run's record.")
		tt.answered("file", "policy", "How many days?", "45 days from delivery.")
		err := tt.tafAsRecorded(ctx)
		return v, log.String(), transcript.String(), err
	}
	v, log, transcript, err := judged("isn't in this run's record")
	if err == nil || !strings.Contains(err.Error(), "answer 2 says") {
		t.Fatalf("a contradicted answer: %v", err)
	}
	if !strings.Contains(transcript, "taf  against its record, answer 2: judged") || strings.Contains(transcript, "answer 1:") || strings.Contains(log, ": judged") {
		t.Fatalf("the verdict's reason, transcript:\n%s\nlog:\n%s", transcript, log)
	}
	records := map[string]int{
		"Record:\n1. retrieve_charge_7f2a {\"charge\":\"ch_1\"} → done\n\nText:\nYes, €7 can be refunded.":                                                                                           0,
		"Record:\n1. (earlier) retrieve_charge_7f2a {\"charge\":\"ch_1\"} → done\n2. create_refund_7f2a {\"charge\":\"ch_1\",\"amount\":700} → " + agent.OutcomeApproved + "\n\nText:\nYou approved": 0,
		"Record:\n(no calls)\n\nText:\n45 days from delivery.": 0,
	}
	for _, p := range v.asked {
		if !strings.Contains(p, eval.Recorded) {
			t.Fatalf("asked without the rule:\n%s", p)
		}
		for r := range records {
			if strings.Contains(p, r) {
				records[r]++
			}
		}
	}
	for r, n := range records {
		if n != eval.Votes {
			t.Errorf("asked %d times about\n%s", n, r)
		}
	}
	if _, log, _, err := judged(); err != nil || !strings.Contains(log, "taf  as recorded: 3 answers") {
		t.Fatalf("all as recorded: %v, log %q", err, log)
	}
	// No judge, no question; answers without their runs are a fault of the test's own.
	tt := &test{tafSaid: []string{"Done."}}
	if err := tt.tafAsRecorded(ctx); err != nil {
		t.Fatalf("without -grade: %v", err)
	}
	tt.cfg.judge = &eval.Judge{Model: &verdicts{}, ModelID: "judge"}
	if err := tt.tafAsRecorded(ctx); err == nil {
		t.Fatal("an answer with no run was judged")
	}
}

// The record's judge on the Claude API, on Eval 42146041790's msg 8: against
// its chat's record, reconstructed from what that run did (the look-up one
// message before, marked earlier, then the refund, held and approved), the
// answer that puts the look-up outside "this run's record" fails, and the
// answer the rules ask for, with what the look-up found in one clause, passes
// (qa-kiff). It costs a few cents, so it runs only when asked: E2E_REGRADE=1
// with ANTHROPIC_API_KEY.
func TestTheRecordJudgeOnLastNightsMsg8(t *testing.T) {
	if os.Getenv("E2E_REGRADE") != "1" || os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("E2E_REGRADE=1 and ANTHROPIC_API_KEY run the record's judge")
	}
	j, err := judgeFrom("claude-sonnet-5-5", "2000000,10000000")
	if err != nil {
		t.Fatal(err)
	}
	record := []eval.RecordedCall{
		{Tool: "retrieve_charge_7f2a", Input: `{"charge":"ch_e2e_checked_1106-0151"}`, Outcome: agent.OutcomeDone, Earlier: true},
		{Tool: "create_refund_7f2a", Input: `{"amount":700,"charge":"ch_e2e_checked_1106-0151","operation_id":"refund-ch_e2e_checked_1106-0151-700-a"}`, Outcome: agent.OutcomeApproved},
	}
	for _, c := range []struct {
		name, file string
		pass       bool
	}{
		{"msg 8, reconstructed", nightMsg8, false},
		{"msg 8 with the look-up in one clause", nightRewrite, true},
	} {
		text := readTafLines(readFile(t, c.file))[7].said
		g, err := j.AskMajority(context.Background(), eval.Recorded, eval.RecordedText(record, text))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: pass %v (%d of %d said otherwise): %s", c.name, g.Pass, g.Split, eval.Votes, g.Why)
		if g.Pass != c.pass {
			t.Errorf("%s: passed %v, want %v: %q", c.name, g.Pass, c.pass, text)
		}
	}
}

// The mode's judge on the Claude API: the same sentence fails before the
// connection reported a mode and passes after it reported that mode; live
// stated as fact fails with test reported; a mode on the desk, which has
// none, fails; and what the old word list read as claims (Evals 42125870057,
// the confirming Eval of #343; #340's question) passes. E2E_REGRADE=1 with
// ANTHROPIC_API_KEY.
func TestTheModeJudgeOnTheSameSentence(t *testing.T) {
	if os.Getenv("E2E_REGRADE") != "1" || os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("E2E_REGRADE=1 and ANTHROPIC_API_KEY run the mode's judge")
	}
	j, err := judgeFrom("claude-sonnet-5-5", "2000000,10000000")
	if err != nil {
		t.Fatal(err)
	}
	reported := modes{test: true, allTest: true}
	same := "Stripe is in test mode. Shall we go on?"
	for _, c := range []struct {
		name   string
		chose  string
		m      modes
		text   string
		before bool
		pass   bool
	}{
		{"said before the report", "test", reported, same, true, false},
		{"said after the report", "test", reported, same, false, true},
		{"the Eval's text 6, after the report, any wording", "test", reported, "Your account there is in test mode.", false, true},
		{"live as fact, test reported", "test", reported, "When I checked, your account was in live mode.", false, false},
		{"a mode on the desk", "", modes{none: true}, "Desk in test mode.", false, false},
		{"the pin as chosen", "test", reported, "Every call is set to test mode, as you chose.", false, true},
		{"the mode question", "", reported, "Test mode or live mode?", true, true},
		{"a next step", "test", reported, "Whether the account itself is test or live, check it before going live.", false, true},
	} {
		var before []string
		if c.before {
			before = []string{c.text}
		}
		g, err := j.AskMajority(context.Background(), modeRule(c.chose, c.m), beforeMarked([]string{c.text}, before))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: pass %v (%d of %d said otherwise): %s", c.name, g.Pass, g.Split, eval.Votes, g.Why)
		if g.Pass != c.pass {
			t.Errorf("%s: passed %v, want %v: %q", c.name, g.Pass, c.pass, c.text)
		}
	}
}
