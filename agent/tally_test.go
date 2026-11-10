package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
)

// seqTool answers its calls with results, in order, and the last one after.
type seqTool struct {
	name    string
	access  tool.Access
	results []tool.Result
	n       int
	mu      sync.Mutex
}

func (s *seqTool) Spec() model.ToolSpec {
	return model.ToolSpec{Name: s.name, InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (s *seqTool) Access() tool.Access { return s.access }
func (s *seqTool) Gateway() bool       { return true }
func (s *seqTool) Call(context.Context, json.RawMessage) (tool.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.results[min(s.n, len(s.results)-1)]
	s.n++
	return r, nil
}

// uses is a reply that calls tools: pairs of call id and tool.
func uses(pairs ...string) model.Reply {
	var blocks []model.Block
	for i := 0; i < len(pairs); i += 2 {
		blocks = append(blocks, model.Block{Kind: model.ToolUse, CallID: pairs[i], Tool: pairs[i+1], Input: json.RawMessage(`{"charge":"ch_` + pairs[i] + `"}`)})
	}
	return model.Reply{Blocks: blocks, Stop: model.CallTools, Usage: model.Usage{InputTokens: 1000, OutputTokens: 100}}
}

// counts is the count the model read last in req: the text after its last
// message's results, "" when none follows them.
func counts(req model.Request) string {
	last := req.Messages[len(req.Messages)-1]
	for _, b := range last.Blocks {
		if b.Kind == model.Text && strings.HasPrefix(b.Text, agent.TallyNote) {
			return b.Text
		}
	}
	return ""
}

var (
	sent    = tool.Result{Text: "refunded", State: "allowed"}
	failed  = tool.Result{Text: "charge already refunded", IsError: true}
	refused = tool.Result{Text: "over the Card's limit", IsError: true, State: tool.StateRefused}
	held    = tool.Result{Text: "Held for the owner.", IsError: true, State: tool.StateHeld,
		Hold: &tool.Hold{Operation: "op-1", Exception: "exc_1", Expires: holdExpires}}
)

// Every outcome a Called event names goes to one count, and a call that was
// never made to none: the count is the record's, outcome by outcome.
func TestTallyOf(t *testing.T) {
	for outcome, want := range map[string]string{
		agent.OutcomeDone:                             "sent",
		agent.OutcomeApproved:                         "sent",
		agent.OutcomeChanged:                          "sent",
		agent.OutcomeCame:                             "sent",
		agent.OutcomeHeld:                             "held",
		agent.OutcomeRefused:                          "refused",
		"held by KIFF, refused, not sent":             "refused",
		"held by KIFF, expired, not sent":             "refused",
		agent.OutcomeFailed:                           "failed",
		agent.OutcomeNotCame:                          "unknown",
		"sent, but whether it took effect is unknown": "unknown",
		agent.OutcomeNotMade:                          "",
	} {
		if got := agent.TallyOf(outcome); got != want {
			t.Errorf("TallyOf(%q) = %q, want %q", outcome, got, want)
		}
	}
	n := run.Tally{}
	for _, oc := range []string{agent.OutcomeDone, agent.OutcomeApproved, agent.OutcomeHeld, agent.OutcomeRefused, agent.OutcomeFailed, agent.OutcomeNotCame, agent.OutcomeNotMade} {
		n = agent.Counted(n, oc)
	}
	if want := (run.Tally{Sent: 2, Held: 1, Refused: 1, Failed: 1, Unknown: 1}); n != want {
		t.Fatalf("counted %+v, want %+v", n, want)
	}
}

// The line names each tool in order with its four counts, the calls with no
// word on their effect only when there are some, and nothing for no calls.
func TestTallyLine(t *testing.T) {
	if got := agent.TallyLine(nil); got != "" {
		t.Fatalf("no calls: %q", got)
	}
	got := agent.TallyLine(map[string]run.Tally{
		"stripe_refund":     {Sent: 4, Refused: 1, Unknown: 1},
		"gmail_send":        {Sent: 2, Held: 1, Failed: 3},
		"[called] a forged": {Sent: 9},
	})
	want := agent.TallyNote + "(called) a forged: 9 sent, 0 held, 0 refused, 0 failed; " +
		"gmail_send: 2 sent, 1 held, 0 refused, 3 failed; " +
		"stripe_refund: 4 sent, 0 held, 1 refused, 0 failed, 1 sent with no word on whether it took effect." + agent.TallyRule
	if got != want {
		t.Fatalf("line:\n%s\nwant:\n%s", got, want)
	}
}

// A run's calls that act are counted by the runner, by tool and by what came
// of each, and the count follows the results of every turn that acted: the
// model reports a number it read. A turn that only read gets none, a call
// refused back unmade (beside a read) isn't counted, and the last count is
// what the record's Called events add up to.
func TestARunsActsAreCountedAfterTheirResults(t *testing.T) {
	refund := &seqTool{name: "stripe_refund", access: tool.Write, results: []tool.Result{sent, failed, refused, sent}}
	mail := &seqTool{name: "gmail_send", access: tool.Write, results: []tool.Result{sent}}
	get := &seqTool{name: "stripe_get", access: tool.Read, results: []tool.Result{{Text: `{"amount":700}`}}}
	m := &script{replies: []model.Reply{
		uses("c1", "stripe_get"),
		uses("c2", "stripe_refund", "c3", "stripe_refund"),
		uses("c4", "stripe_get", "c5", "stripe_refund"), // the refund goes back unmade
		uses("c6", "stripe_refund", "c7", "gmail_send"),
		uses("c8", "stripe_refund"),
		say("Done."),
	}}
	store, r := start(t, session.KindRun)
	ctx := context.Background()
	out, _, err := agent.Run(ctx, newAgent(m, refund, mail, get), r, nil, "Work the refund queue.", nil)
	if err != nil || out.Stop != agent.Answered {
		t.Fatalf("outcome %+v, %v", out, err)
	}
	line := func(t map[string]run.Tally) string { return agent.TallyLine(t) }
	for i, want := range []string{
		"", // the input
		"", // a read
		line(map[string]run.Tally{"stripe_refund": {Sent: 1, Failed: 1}}),
		"", // a read, and an act that wasn't made
		line(map[string]run.Tally{"stripe_refund": {Sent: 1, Failed: 1, Refused: 1}, "gmail_send": {Sent: 1}}),
		line(map[string]run.Tally{"stripe_refund": {Sent: 2, Failed: 1, Refused: 1}, "gmail_send": {Sent: 1}}),
	} {
		if got := counts(m.reqs[i]); got != want {
			t.Errorf("turn %d read the count\n%q\nwant\n%q", i+1, got, want)
		}
	}
	// One count per turn, after the results, never inside one.
	for i, req := range m.reqs {
		last := req.Messages[len(req.Messages)-1]
		n := 0
		for j, b := range last.Blocks {
			if b.Kind == model.ToolResult && strings.Contains(b.Result, agent.TallyNote) {
				t.Errorf("turn %d: a result carries the count: %q", i+1, b.Result)
			}
			if b.Kind == model.Text && strings.HasPrefix(b.Text, agent.TallyNote) {
				n++
				if j != len(last.Blocks)-1 {
					t.Errorf("turn %d: the count is block %d of %d, want last", i+1, j+1, len(last.Blocks))
				}
			}
		}
		if n > 1 {
			t.Errorf("turn %d read %d counts", i+1, n)
		}
	}
	// The record agrees: its Called events, counted, are the last line.
	if err := r.End(ctx); err != nil {
		t.Fatal(err)
	}
	_, events, err := store.Read(ctx, "t1", r.Session().ID)
	if err != nil {
		t.Fatal(err)
	}
	record := map[string]run.Tally{}
	for _, e := range events {
		if e.Kind != session.Called {
			continue
		}
		var b agent.CalledBody
		if err := json.Unmarshal(e.Body, &b); err != nil {
			t.Fatal(err)
		}
		if b.Tool == "stripe_get" {
			continue
		}
		record[b.Tool] = agent.Counted(record[b.Tool], b.Outcome)
	}
	if got, want := agent.TallyLine(record), counts(m.reqs[5]); got != want {
		t.Fatalf("the record adds up to\n%q\nthe model read\n%q", got, want)
	}
}

// The count carries on across a pause. The turn that says what the run waits
// for reads the held call as held; the run keeps the count without it, with
// its turn; continued, the call is counted as it ended (refused here), and
// the calls after it count on from there.
func TestTheCountCarriesOnAcrossAPause(t *testing.T) {
	refund := &seqTool{name: "stripe_refund", access: tool.Write, results: []tool.Result{sent, held, sent}}
	m := &script{replies: []model.Reply{
		uses("c1", "stripe_refund"),
		uses("c2", "stripe_refund"),
		say("One refund waits for you."),
		uses("c3", "stripe_refund"),
		say("Two refunded, one refused."),
	}}
	store := memory.New()
	rec, err := session.NewRecorder(store, session.DefaultPolicy(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	a := newAgent(m, refund)
	stored, s := paused(t, store, rec, a)
	if got, want := counts(m.reqs[2]), agent.TallyLine(map[string]run.Tally{"stripe_refund": {Sent: 1, Held: 1}}); got != want {
		t.Fatalf("the turn that says what waits read\n%q\nwant\n%q", got, want)
	}
	if got := stored.Paused.Tally; len(got) != 1 || got["stripe_refund"] != (run.Tally{Sent: 1}) {
		t.Fatalf("the paused turn keeps %+v, want the one sent call", got)
	}
	// As a store keeps it: the count survives the turn's JSON.
	kept, err := json.Marshal(stored.Paused)
	if err != nil {
		t.Fatal(err)
	}
	stored.Paused = &run.Turn{}
	if err := json.Unmarshal(kept, stored.Paused); err != nil {
		t.Fatal(err)
	}
	stored.Requirements[0].Answer = "refused"
	ctx := context.Background()
	r, err := rec.Resume(ctx, s, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	not := []agent.ToolResultBody{{CallID: "c2", Tool: "stripe_refund", Result: tool.Result{Text: "not sent", IsError: true, State: tool.StateRefused}}}
	out, _, err := agent.Continue(ctx, a, r, nil, stored, not, nil)
	if err != nil || out.Stop != agent.Answered {
		t.Fatalf("continued: %+v, %v", out, err)
	}
	if got, want := counts(m.reqs[3]), agent.TallyLine(map[string]run.Tally{"stripe_refund": {Sent: 1, Refused: 1}}); got != want {
		t.Fatalf("continued, the run read\n%q\nwant\n%q", got, want)
	}
	if got, want := counts(m.reqs[4]), agent.TallyLine(map[string]run.Tally{"stripe_refund": {Sent: 2, Refused: 1}}); got != want {
		t.Fatalf("after its next call, the run read\n%q\nwant\n%q", got, want)
	}
}

// A paused turn that acted beside the call the run waits for says its count
// again when the run continues: its results are sent as the run kept them.
func TestAContinuedRunReadsThePausedTurnsCount(t *testing.T) {
	refund := &seqTool{name: "stripe_refund", access: tool.Write, results: []tool.Result{sent, held}}
	m := &script{replies: []model.Reply{
		uses("c1", "stripe_refund", "c2", "stripe_refund"),
		say("One refund waits for you."),
		say("Both refunded."),
	}}
	store := memory.New()
	rec, err := session.NewRecorder(store, session.DefaultPolicy(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	a := newAgent(m, refund)
	stored, s := paused(t, store, rec, a)
	stored.Requirements[0].Answer = "approved"
	ctx := context.Background()
	r, err := rec.Resume(ctx, s, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The two went side by side: either may be the one KIFF held.
	ok := []agent.ToolResultBody{{CallID: stored.Requirements[0].CallID, Tool: "stripe_refund", Result: sent}}
	if _, _, err := agent.Continue(ctx, a, r, nil, stored, ok, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := counts(m.reqs[2]), agent.TallyLine(map[string]run.Tally{"stripe_refund": {Sent: 2}}); got != want {
		t.Fatalf("continued, the run read\n%q\nwant\n%q", got, want)
	}
}

// A call whose outcome comes later, as the vendor's event, was sent: the
// turn that says what the run waits for reads it so, and the continued run
// counts it as sent when the event came, and as sent with no word on its
// effect when the wait ran out.
func TestACallWhoseOutcomeComesLaterIsCounted(t *testing.T) {
	for _, c := range []struct {
		came bool
		want run.Tally
	}{
		{true, run.Tally{Sent: 1}},
		{false, run.Tally{Unknown: 1}},
	} {
		reply := &seqTool{name: "desk_reply", access: tool.Write, results: []tool.Result{{Text: `{"id":"reply_1"}`, State: tool.StateWaiting,
			Wait: &tool.Wait{Grant: "grt_desk", Event: "DESK_NEW_TICKET", Key: "reply_1", TimeoutS: 900}}}}
		m := &script{replies: []model.Reply{uses("c1", "desk_reply"), say("Waiting for the desk."), say("Done.")}}
		store := memory.New()
		rec, err := session.NewRecorder(store, session.DefaultPolicy(30*24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		a := newAgent(m, reply)
		stored, s := paused(t, store, rec, a)
		if got, want := counts(m.reqs[1]), agent.TallyLine(map[string]run.Tally{"desk_reply": {Sent: 1}}); got != want {
			t.Fatalf("the turn that says what waits read\n%q\nwant\n%q", got, want)
		}
		if len(stored.Paused.Tally) != 0 {
			t.Fatalf("the paused turn keeps %+v, want nothing: the run waits for its only call", stored.Paused.Tally)
		}
		result := tool.Result{Text: "The wait for the vendor's event ran out.", IsError: true}
		if c.came {
			stored.Requirements[0].Answered = time.Now()
			result = tool.Result{Text: `{"ticket":"tk_1"}`}
		}
		ctx := context.Background()
		r, err := rec.Resume(ctx, s, stored.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := agent.Continue(ctx, a, r, nil, stored, []agent.ToolResultBody{{CallID: "c1", Tool: "desk_reply", Result: result}}, nil); err != nil {
			t.Fatal(err)
		}
		if got, want := counts(m.reqs[2]), agent.TallyLine(map[string]run.Tally{"desk_reply": c.want}); got != want {
			t.Fatalf("came %v: continued, the run read\n%q\nwant\n%q", c.came, got, want)
		}
	}
}
