package cases_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/eval"
	"github.com/kiff/agents/eval/cases"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
)

// outcome is what came of one call, as the runner records it.
type outcome struct {
	tool, input, what string
}

// recordedRun is r with what the runner recorded of its calls (Called
// events), the count the run kept, and the writes that went through.
func recordedRun(r eval.Run, tally map[string]run.Tally, done []run.Done, called ...outcome) eval.Run {
	for _, c := range called {
		raw, _ := json.Marshal(agent.CalledBody{Tool: c.tool, Input: json.RawMessage(c.input), Outcome: c.what})
		r.Events = append(r.Events, session.Event{Session: "s", Seq: int64(len(r.Events) + 1), Kind: session.Called, Body: raw})
	}
	r.Tally, r.Done = tally, done
	return r
}

func check(res eval.Result, name string) eval.CheckResult {
	i := slices.IndexFunc(res.Checks, func(c eval.CheckResult) bool { return c.Name == name })
	if i < 0 {
		return eval.CheckResult{Name: "missing: " + name}
	}
	return res.Checks[i]
}

const recordCheck = "says what was done as the record has it"

// What the run did, as the hand-off sender's first Eval run had it: evidence
// sent on one dispute, another accepted, and two reads KIFF refused.
func senderRecord() []outcome {
	return []outcome{
		{"list_disputes", `{"status":"needs_response"}`, agent.OutcomeDone},
		{"shop_get_order", `{"order":"T-2001"}`, agent.OutcomeRefused},
		{"shop_get_order", `{"order":"T-2002"}`, agent.OutcomeRefused},
		{"update_dispute", `{"dispute":"dp_2001","submit":true}`, agent.OutcomeDone},
		{"close_dispute", `{"dispute":"dp_2002"}`, agent.OutcomeDone},
	}
}

func senderTally() map[string]run.Tally {
	return map[string]run.Tally{"shop_get_order": {Refused: 2}, "update_dispute": {Sent: 1}, "close_dispute": {Sent: 1}}
}

var senderDone = []run.Done{
	{CallID: "2", Tool: "update_dispute", Input: json.RawMessage(`{"dispute":"dp_2001","submit":true}`), Resource: "dp_2001"},
	{CallID: "3", Tool: "close_dispute", Resource: "dp_2002"},
}

// A run that keeps its count is graded on both halves of what the owner
// reads: the count, in code, against what the runner recorded of each call,
// and the sentences beside it, judged. A sentence a model wrote wrong twice
// ("six submitted", "none refused", Eval 43243736555) fails the gate even
// under a count that is right, and a wrong count fails it under a sentence
// that is right.
func TestTheGateReadsTheRunsCountAndItsSentences(t *testing.T) {
	const words = "its words don't contradict the record"
	wrong := "Answered T-2001 and accepted T-2002. Nothing was held or refused.\n\n## Hand-off\nAccepted T-2002, $12."
	base := runOf(wrong, goodSender()...)
	contradicted := &eval.Judge{Model: verdict(false)} // the judge finds the sentence wrong
	agreed := &eval.Judge{Model: verdict(true)}

	// The count is the record's and the sentence isn't: the count's check passes, the words' fails the gate.
	res := grade(t, cases.HandoffSender, disputeIDs, recordedRun(base, senderTally(), senderDone, senderRecord()...), contradicted)
	if got := check(res, recordCheck); !got.Pass || got.Advisory {
		t.Fatalf("a count that is the record's: %+v", got)
	}
	if got := check(res, words); got.Pass || got.Advisory || got.Skipped {
		t.Fatalf("the wrong sentence didn't fail as a gate check: %+v", got)
	}
	if got := failed(res); !slices.Contains(got, words) || slices.Contains(got, recordCheck) {
		t.Fatalf("failed %v", got)
	}
	// The sentence is right and the count isn't: the other way round.
	short := senderTally()
	delete(short, "shop_get_order")
	res = grade(t, cases.HandoffSender, disputeIDs, recordedRun(base, short, senderDone, senderRecord()...), agreed)
	if got := failed(res); !slices.Contains(got, recordCheck) || slices.Contains(got, words) {
		t.Fatalf("a count without its refused reads: failed %v", got)
	}
	// Both right: neither fails.
	res = grade(t, cases.HandoffSender, disputeIDs, recordedRun(base, senderTally(), senderDone, senderRecord()...), agreed)
	if got := failed(res); slices.Contains(got, recordCheck) || slices.Contains(got, words) {
		t.Fatalf("both right: failed %v", got)
	}

	// A run that keeps no count has only its words: they are judged once, by the first check.
	res = grade(t, cases.HandoffSender, disputeIDs, base, contradicted)
	if got := check(res, recordCheck); got.Pass || got.Advisory {
		t.Fatalf("no count kept, and the wrong sentence passed the gate: %+v", got)
	}
	if got := check(res, words); !got.Skipped {
		t.Fatalf("judged twice with no count kept: %+v", got)
	}
}

// Each way the run's count can differ from the record of its calls fails the
// gate, and says which tool.
func TestACountThatIsntTheRecordsFails(t *testing.T) {
	judge := &eval.Judge{Model: verdict(true)}
	base := runOf(goodSenderResult, goodSender()...)
	graded := func(tally map[string]run.Tally, called ...outcome) eval.CheckResult {
		return check(grade(t, cases.HandoffSender, disputeIDs, recordedRun(base, tally, senderDone, called...), judge), recordCheck)
	}
	if got := graded(senderTally(), senderRecord()...); !got.Pass {
		t.Fatalf("the record's own count failed: %s", got.Why)
	}
	with := func(change func(map[string]run.Tally)) map[string]run.Tally {
		t := senderTally()
		change(t)
		return t
	}
	for name, tt := range map[string]struct {
		tally map[string]run.Tally
		want  string
	}{
		// Sighting 2: the refused reads left out of the count.
		"refused reads left out": {with(func(t map[string]run.Tally) { delete(t, "shop_get_order") }), "shop_get_order"},
		"one refused read short": {with(func(t map[string]run.Tally) { t["shop_get_order"] = run.Tally{Refused: 1} }), "shop_get_order"},
		// The count started again from nothing after a pause: a write that went through is missing.
		"a sent write missing":    {with(func(t map[string]run.Tally) { delete(t, "close_dispute") }), "close_dispute"},
		"a write counted twice":   {with(func(t map[string]run.Tally) { t["update_dispute"] = run.Tally{Sent: 2} }), "update_dispute"},
		"a read counted as sent":  {with(func(t map[string]run.Tally) { t["list_disputes"] = run.Tally{Sent: 1} }), "list_disputes"},
		"a tool never called":     {with(func(t map[string]run.Tally) { t["create_refund"] = run.Tally{Sent: 1} }), "create_refund"},
		"sent counted as refused": {with(func(t map[string]run.Tally) { t["close_dispute"] = run.Tally{Refused: 1} }), "close_dispute"},
		// A tool the scenario names nowhere is counted as a write or as a read, never as something else.
		"an unnamed tool miscounted": {with(func(t map[string]run.Tally) { t["shop_get_order"] = run.Tally{Refused: 2, Failed: 1} }), "shop_get_order"},
		"an empty count, not none":   {map[string]run.Tally{}, "close_dispute"},
	} {
		got := graded(tt.tally, senderRecord()...)
		if got.Pass || !strings.Contains(got.Why, tt.want) {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

// A tool the scenario names nowhere (a vendor's read whose name doesn't begin
// with a read verb) is counted as the runner decided: as a read, with only its
// refusals, or as a write, with everything. The check doesn't rule on the tool.
func TestAToolTheScenarioDoesntNameIsCountedEitherWay(t *testing.T) {
	judge := &eval.Judge{Model: verdict(true)}
	base := runOf(goodSenderResult, goodSender()...)
	record := append(senderRecord(), outcome{"shop_get_order", `{"order":"T-2003"}`, agent.OutcomeDone})
	graded := func(orders run.Tally) eval.CheckResult {
		tally := senderTally()
		tally["shop_get_order"] = orders
		return check(grade(t, cases.HandoffSender, disputeIDs, recordedRun(base, tally, senderDone, record...), judge), recordCheck)
	}
	if got := graded(run.Tally{Refused: 2}); !got.Pass {
		t.Errorf("counted as a read: %s", got.Why)
	}
	if got := graded(run.Tally{Refused: 2, Sent: 1}); !got.Pass {
		t.Errorf("counted as a write: %s", got.Why)
	}
	if got := graded(run.Tally{Refused: 1, Sent: 1}); got.Pass {
		t.Error("a refusal left out passed")
	}
	// A tool whose name says it reads is held to a read's count, and one the scenario acts with to a write's.
	sent := senderTally()
	sent["list_disputes"] = run.Tally{Sent: 1}
	if got := check(grade(t, cases.HandoffSender, disputeIDs, recordedRun(base, sent, senderDone, record...), judge), recordCheck); got.Pass {
		t.Error("a read that went through was counted as sent, and passed")
	}
	left := senderTally()
	delete(left, "update_dispute")
	if got := check(grade(t, cases.HandoffSender, disputeIDs, recordedRun(base, left, senderDone, record...), judge), recordCheck); got.Pass {
		t.Error("a write that went through was left out, and passed")
	}
}

// What the count leaves out and takes in, as the runner does: a call still
// held for the owner isn't counted until it ends, and its answer takes its
// place; a failed write and one sent with no word on its effect are counted
// as that; a read that failed isn't counted.
func TestTheCountFollowsEachCallToItsEnd(t *testing.T) {
	judge := &eval.Judge{Model: verdict(true)}
	base := runOf(goodChargebackReport, goodChargebacks()...)
	graded := func(tally map[string]run.Tally, called ...outcome) eval.CheckResult {
		return check(grade(t, cases.Chargebacks, disputeIDs, recordedRun(base, tally, nil, called...), judge), recordCheck)
	}
	sent := outcome{"update_dispute", `{"dispute":"dp_2001"}`, agent.OutcomeDone}
	held := outcome{"update_dispute", `{"dispute":"dp_2004"}`, agent.OutcomeHeld}
	refused := outcome{"close_dispute", `{"dispute":"dp_2005"}`, agent.OutcomeRefused}

	// Paused on the hold: the held call is in neither count.
	if got := graded(map[string]run.Tally{"update_dispute": {Sent: 1}, "close_dispute": {Refused: 1}}, sent, held, refused); !got.Pass {
		t.Errorf("paused on a hold: %s", got.Why)
	}
	if got := graded(map[string]run.Tally{"update_dispute": {Sent: 1, Held: 1}, "close_dispute": {Refused: 1}}, sent, held, refused); got.Pass {
		t.Error("a call still held was counted, and passed")
	}
	// The owner approved it: recorded again, with the same input, as done. One call, sent.
	approved := outcome{"update_dispute", `{ "dispute": "dp_2004" }`, agent.OutcomeApproved}
	if got := graded(map[string]run.Tally{"update_dispute": {Sent: 2}, "close_dispute": {Refused: 1}}, sent, held, refused, approved); !got.Pass {
		t.Errorf("a hold approved: %s", got.Why)
	}
	// A write the vendor failed, one with no word on its effect, and a read that failed.
	failedWrite := outcome{"update_dispute", `{"dispute":"dp_2003"}`, agent.OutcomeFailed}
	unknown := outcome{"update_dispute", `{"dispute":"dp_2006"}`, "sent, with no word on whether it took effect"}
	failedRead := outcome{"retrieve_dispute", `{"dispute":"dp_2007"}`, agent.OutcomeFailed}
	if got := graded(map[string]run.Tally{"update_dispute": {Sent: 1, Failed: 1, Unknown: 1}}, sent, failedWrite, unknown, failedRead); !got.Pass {
		t.Errorf("failed and unknown: %s", got.Why)
	}
	if got := graded(map[string]run.Tally{"update_dispute": {Sent: 1, Failed: 1, Unknown: 1}, "retrieve_dispute": {Failed: 1}}, sent, failedWrite, unknown, failedRead); got.Pass {
		t.Error("a failed read was counted, and passed")
	}
}

// The sender's gate reads what is handed to the next taf, which tafs cloud
// makes from the run's record as well as its words. A taf that accepted a
// dispute and then wrote "Nothing to hand over" (Eval 43249509530) still hands
// that dispute on, from its record: the gate passes, and its own sentence is
// failed advice.
func TestWhatIsHandedComesFromTheRecordWhenTheTafSaysNothing(t *testing.T) {
	judge := &eval.Judge{Model: verdict(true)}
	const handsOn, writes = "ends its result with the hand-off", "writes the hand-off itself"
	saysNothing := "Answered T-2001; accepted T-2002.\n\n## Hand-off\nNothing to hand over: both are answered."
	with := func(result string, done []run.Done) eval.Result {
		return grade(t, cases.HandoffSender, disputeIDs, recordedRun(runOf(result, goodSender()...), senderTally(), done, senderRecord()...), judge)
	}

	res := with(saysNothing, senderDone)
	if got := check(res, handsOn); !got.Pass || got.Advisory {
		t.Fatalf("the record names what was accepted: %+v", got)
	}
	if got := check(res, writes); got.Pass || !got.Advisory {
		t.Fatalf("the taf's own sentence isn't failed advice: %+v", got)
	}
	// Its record shows no write that went through: nothing is handed, and the gate fails.
	if got := check(with(saysNothing, nil), handsOn); got.Pass || !strings.Contains(got.Why, "nothing is handed") {
		t.Errorf("nothing handed, with T-2002 to hand over: %+v", got)
	}
	// The record's lines don't name the item: only another write went through.
	other := []run.Done{{Tool: "update_dispute", Resource: "dp_2001"}}
	if got := check(with(saysNothing, other), handsOn); got.Pass || !strings.Contains(got.Why, "T-2002") {
		t.Errorf("handed from a record without T-2002: %+v", got)
	}
	// KIFF named no resource for the call: the record's line is the tool alone, as tafs cloud hands it,
	// so the next taf isn't told which dispute. The call's input isn't handed.
	byInput := []run.Done{{Tool: "close_dispute", Input: json.RawMessage(`{"dispute":"dp_2002"}`)}}
	if got := check(with(saysNothing, byInput), handsOn); got.Pass || !strings.Contains(got.Why, "T-2002") {
		t.Errorf("a line with no resource passed for T-2002: %+v", got)
	}
	// No section at all: nothing is handed, whatever the record shows.
	if got := check(with("Answered T-2001; accepted T-2002.", senderDone), handsOn); got.Pass {
		t.Error("no section, and the gate passed")
	}
	// The taf wrote it right: both pass.
	res = with(goodSenderResult, senderDone)
	if !check(res, handsOn).Pass || !check(res, writes).Pass {
		t.Errorf("a good hand-off: %+v, %+v", check(res, handsOn), check(res, writes))
	}
	// Its own hand-off is what's handed even when the record shows more: it leaves T-2002 out, and fails.
	if got := check(with("Done.\n\n## Hand-off\nOne dispute accepted, for $12.", senderDone), handsOn); got.Pass {
		t.Error("a hand-off that doesn't name T-2002 passed on the record's word")
	}
}

// With nothing open, nothing is handed and the taf says so: a run whose
// record shows a write that went through hands it on, which isn't nothing.
func TestNothingToHandOverIsCheckedAgainstTheRecord(t *testing.T) {
	judge := &eval.Judge{Model: verdict(true)}
	const handsOn = "ends its result with the hand-off"
	none := []call{kiff("kiff_pending", `{}`), {tool: "list_disputes", server: "kiff", input: `{"status": "needs_response"}`, result: `[]`}}
	good := "No dispute is open.\n\n## Hand-off\nNothing to hand over\nNo dispute was open, so none was accepted."
	if got := check(grade(t, cases.HandoffNothing, nil, recordedRun(runOf(good, none...), map[string]run.Tally{}, nil), judge), handsOn); !got.Pass {
		t.Fatalf("nothing done, and said so: %s", got.Why)
	}
	wrote := []run.Done{{Tool: "close_dispute", Resource: "dp_2002"}}
	if got := check(grade(t, cases.HandoffNothing, nil, recordedRun(runOf(good, none...), map[string]run.Tally{}, wrote), judge), handsOn); got.Pass || !strings.Contains(got.Why, "record shows writes") {
		t.Errorf("a write went through with nothing to do: %+v", got)
	}
}
