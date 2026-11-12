package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/eval"
	"github.com/kiff/agents/session"
)

// tafAsRecorded is the gate's judged check of the taf's answers (the owner,
// 6 Nov 2026: the gate tests results, and the sentences that could mislead
// about them): no answer says something was done, or not done, that its
// record contradicts (eval.Recorded), by majority. An answer's record is its
// chat's calls up to its run's: a look-up one message before counts, marked
// "earlier" (qa-kiff). A number an answer gives of its calls is checked too,
// against the record's counts by tool (eval.RecordedCall.Count). The judges'
// reasons may quote the chat, so they go to the transcript, after the taf's
// answers; the log counts them.
func (t *test) tafAsRecorded(ctx context.Context) error {
	if t.cfg.judge == nil || len(t.tafSaid) == 0 {
		return nil
	}
	if len(t.tafRuns) != len(t.tafSaid) {
		return fmt.Errorf("%d of the taf's answers, from %d runs: each needs its run for its record", len(t.tafSaid), len(t.tafRuns))
	}
	acting, err := t.tafActs(ctx)
	if err != nil {
		return err
	}
	chats := map[string][]session.Event{}
	var failed []string
	for i, text := range t.tafSaid {
		at := t.tafRuns[i]
		events, ok := chats[at.session]
		if !ok {
			_, ev, err := t.sessions.Read(ctx, t.tenant, at.session)
			if err != nil {
				return fmt.Errorf("reading the taf's chat for its record: %w", err)
			}
			events, chats[at.session] = ev, ev
		}
		calls, err := calledUpTo(events, at.run, acting)
		if err != nil {
			return fmt.Errorf("answer %d's record: %w", i+1, err)
		}
		g, err := t.cfg.judge.AskMajority(ctx, eval.Recorded, eval.RecordedText(calls, text))
		if err != nil {
			return err
		}
		if g.Split > 0 {
			fmt.Fprintf(t.cfg.log, "taf  warn as recorded, answer %d: %d of %d judges said otherwise\n", i+1, g.Split, eval.Votes)
		}
		if !g.Pass {
			t.logf("taf  against its record, answer %d: %s", i+1, g.Why)
			failed = append(failed, fmt.Sprint(i+1))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("the taf's answer %s says something was done, or not, that its record contradicts, as judged (the transcript says why)", strings.Join(failed, ", "))
	}
	fmt.Fprintf(t.cfg.log, "taf  as recorded: %d answers\n", len(t.tafSaid))
	return nil
}

// calledUpTo is the record a judge reads beside the answer run gave: its
// chat's calls up to and including run's, in order, those of earlier runs
// marked Earlier. A held call is recorded twice, held and then answered
// (agent.outcome): its answer takes the hold's place, so the call is listed
// once, as it ended. Only a call of a tool in acting is counted
// (eval.RecordedCall.Count), as the runner's count for the taf takes in
// only the calls that act: a look-up is in neither, so "I looked up 3
// charges" after one list call is no miscount.
func calledUpTo(events []session.Event, run string, acting map[string]bool) ([]eval.RecordedCall, error) {
	last := -1
	for i, e := range events {
		if e.Run == run {
			last = i
		}
	}
	if last < 0 {
		return nil, errors.New("the run isn't in its chat's events")
	}
	type entry struct {
		run  string
		body agent.CalledBody
		held bool
	}
	var entries []entry
	for _, e := range events[:last+1] {
		if e.Kind != session.Called {
			continue
		}
		var b agent.CalledBody
		if err := json.Unmarshal(e.Body, &b); err != nil {
			return nil, err
		}
		answered := false
		if b.Outcome != agent.OutcomeHeld {
			for i := range entries {
				if h := &entries[i]; h.held && h.run == e.Run && h.body.Tool == b.Tool && sameInput(h.body.Input, b.Input) {
					h.body.Outcome, h.held, answered = b.Outcome, false, true
					break
				}
			}
		}
		if !answered {
			entries = append(entries, entry{run: e.Run, body: b, held: b.Outcome == agent.OutcomeHeld})
		}
	}
	calls := make([]eval.RecordedCall, len(entries))
	for i, en := range entries {
		calls[i] = eval.RecordedCall{Tool: en.body.Tool, Input: string(en.body.Input), Outcome: en.body.Outcome, Earlier: en.run != run}
		if acting[en.body.Tool] {
			calls[i].Count = countOf(en.body.Outcome)
		}
	}
	return calls, nil
}

// tafActs is the taf's tools that act, by the names its calls are recorded
// under: every tool of the grants this run connected that Setup didn't mark
// as only reading. Empty with no grant to read (the judge then gets no
// counts, and checks no number).
func (t *test) tafActs(ctx context.Context) (map[string]bool, error) {
	if t.phone == nil || len(t.grants) == 0 {
		return nil, nil
	}
	var grants []grant
	for _, id := range t.grants {
		g, err := t.phone.Grant(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("reading the taf's tools for its record's counts: %w", err)
		}
		grants = append(grants, g)
	}
	return actingTools(grants), nil
}

// actingTools is the tools of grants that act, under both of their names.
func actingTools(grants []grant) map[string]bool {
	acting := map[string]bool{}
	for _, g := range grants {
		for _, tl := range g.Request.Tools {
			if tl.ReadOnly || tl.SetupReads {
				continue
			}
			acting[tl.Tool] = true
			if tl.Name != "" {
				acting[tl.Name] = true
			}
		}
	}
	return acting
}

// countOf is the count a recorded call adds to, in the record's words, as
// the runner counts it for the taf (agent.TallyOf): the judge checks the
// taf's numbers against the same count the taf was shown.
func countOf(outcome string) string {
	if c := agent.TallyOf(outcome); c != "sent" {
		return c
	}
	return "done"
}

// sameInput is whether two inputs are the same JSON, however stored: the
// run's store may give a held call's input back with its keys reordered.
func sameInput(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return string(a) == string(b)
	}
	return reflect.DeepEqual(x, y)
}
