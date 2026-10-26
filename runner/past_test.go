package runner_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/registry/registrytest"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/runner"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool"
)

// readTool is a read-only tool through KIFF's gateway.
type readTool struct {
	name, answer string
	calls        int
}

func (r *readTool) Spec() model.ToolSpec {
	return model.ToolSpec{Name: r.name, InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (r *readTool) Access() tool.Access { return tool.Read }
func (r *readTool) Gateway() bool       { return true }
func (r *readTool) Call(context.Context, json.RawMessage) (tool.Result, error) {
	r.calls++
	return tool.Result{Text: r.answer}, nil
}

// panelTool shows the owner a component naming a grant, and says where each
// grant the session's components name stands now (a tool.Stater).
type panelTool struct {
	shown [][]json.RawMessage // what each State was given
}

func (p *panelTool) Spec() model.ToolSpec {
	return model.ToolSpec{Name: "panel", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (p *panelTool) Access() tool.Access { return tool.Own }
func (p *panelTool) Gateway() bool       { return true }
func (p *panelTool) Shows() bool         { return true }
func (p *panelTool) Call(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{Text: "shown", Component: json.RawMessage(`{"type":"approval","id":"a1","grant":"g1"}`)}, nil
}
func (p *panelTool) State(_ context.Context, shown []json.RawMessage) (string, error) {
	p.shown = append(p.shown, shown)
	var b strings.Builder
	for _, g := range tool.Named(shown, "grant", 8) {
		b.WriteString("grant " + g + ": connected")
	}
	return b.String(), nil
}

// lastAsk is the text of the user message that ends req: the run's input,
// with its preface.
func lastAsk(req model.Request) string {
	m := req.Messages[len(req.Messages)-1]
	var parts []string
	for _, b := range m.Blocks {
		if b.Kind == model.Text {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// keeping is the fixture with a read tool, its agent keeping a chat's reads
// for an hour idle and four hours at most, and a sealer.
func keeping(t *testing.T, replies ...model.Reply) (*fixture, *readTool) {
	f := newFixture(t, replies...)
	read := &readTool{name: "get_charge", answer: "charge ch_1: 50.00 EUR, refunded 0"}
	f.extra = []tool.Tool{read}
	d := registrytest.Definition("t1", "rae")
	d.Keep[session.KeptRead] = session.Rule{Keep: session.UntilIdle, Retention: time.Hour, Max: 4 * time.Hour}
	if err := f.agents.Put(ctx, d); err != nil {
		t.Fatal(err)
	}
	f.r.Sealer = &session.KeySealer{Keys: session.MemoryKeys()}
	return f, read
}

func (f *fixture) execute(r run.Run) run.Run {
	f.t.Helper()
	got, err := f.r.Execute(ctx, "t1", r.ID, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

// A chat's next message reads what was said, never what a tool returned:
// nothing of a read is kept, and each chat run is told to name what a
// follow-up acts on.
func TestAChatsNextRunReadsWhatWasSaidNotWhatToolsReturned(t *testing.T) {
	f, _ := keeping(t, use("c1", "get_charge", `{"charge":"ch_1"}`), say("It's 50 € (charge ch_1)."), say("Refunding ch_1."))
	r1 := f.execute(f.start(server.Start{Input: "how much was ch_1?"}))
	f.clock.add(10 * time.Minute)
	f.execute(f.start(server.Start{Session: r1.Session, Input: "refund it"}))

	reqs := f.model.requests()
	if !strings.HasSuffix(reqs[0].System, runner.NamesNote) {
		t.Fatalf("a chat run isn't told to name what a follow-up acts on: %q", reqs[0].System)
	}
	second := reqs[2]
	for _, m := range second.Messages {
		for _, b := range m.Blocks {
			if strings.Contains(b.Text+b.Result, "refunded 0") {
				t.Fatalf("the next message carries a tool's result: %+v", b)
			}
		}
	}
	if !strings.Contains(fmt.Sprint(second.Messages), "It's 50 € (charge ch_1).") {
		t.Fatal("the next message doesn't read what was said")
	}
	if ask := lastAsk(second); !strings.HasSuffix(ask, "refund it") {
		t.Fatalf("the input comes last: %q", ask)
	}
	if slices.Contains(f.events(r1), session.KeptRead) {
		t.Fatal("a read was kept")
	}
	// What the owner sees of the run is unchanged: their message, not the preface.
	_, events, err := f.store.Read(ctx, "t1", r1.Session)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		var b agent.TextBody
		if e.Kind == session.OwnerMessage && json.Unmarshal(e.Body, &b) == nil && b.Text != "how much was ch_1?" && b.Text != "refund it" {
			t.Fatalf("the owner's message was recorded as %q", b.Text)
		}
	}
}

func TestAJobsRunIsNotToldAboutFollowUps(t *testing.T) {
	f, _ := keeping(t, say("done"))
	f.execute(f.start(server.Start{Tenant: "t1", Agent: "rae", Trigger: run.BySchedule, Input: "reconcile"}))
	if s := f.model.requests()[0].System; strings.Contains(s, runner.NamesNote) {
		t.Fatalf("a job's run was told about follow-ups: %q", s)
	}
}

func TestARunIsToldWhereWhatItsChatShowedStandsNow(t *testing.T) {
	f := newFixture(t, use("c1", "panel", `{}`), say("Approve the connection."), say("Connected, so next…"))
	panel := &panelTool{}
	f.extra = []tool.Tool{panel}
	r := f.execute(f.start(server.Start{Input: "set up refunds"}))
	f.execute(f.start(server.Start{Session: r.Session, Input: "[approval a1] approved"}))

	ask := lastAsk(f.model.requests()[2])
	if !strings.Contains(ask, runner.StateNote) || !strings.Contains(ask, "grant g1: connected") {
		t.Fatalf("the run after the approval wasn't told where the grant stands: %q", ask)
	}
	if !strings.HasSuffix(ask, "[approval a1] approved") {
		t.Fatalf("the owner's answer comes last: %q", ask)
	}
	// The first run's chat had shown nothing yet: no state, no lookup.
	if first := lastAsk(f.model.requests()[0]); strings.Contains(first, runner.StateNote) {
		t.Fatalf("the first run got a state with nothing shown: %q", first)
	}
}

func TestEarlierRunsThatNoLongerFitAreSummarised(t *testing.T) {
	long := func(s string) string { return s + strings.Repeat(" and more", 20) } // about 50 tokens each
	f := newFixture(t, say("one"), say("two"), say("three"), say("SUMMARY of one and two"), say("four"), say("five"))
	d := registrytest.Definition("t1", "rae")
	d.History, d.HistoryTokens = 50, 130
	if err := f.agents.Put(ctx, d); err != nil {
		t.Fatal(err)
	}
	var runs []run.Run
	r := f.execute(f.start(server.Start{Input: long("refund up to 50 € each, 100 € a day")}))
	runs = append(runs, r)
	for _, in := range []string{long("b"), long("c"), long("d")} {
		runs = append(runs, f.execute(f.start(server.Start{Session: r.Session, Input: in})))
	}
	reqs := f.model.requests()
	if reqs[3].System != agent.SummarySystem || len(reqs[3].Tools) != 0 {
		t.Fatalf("the fourth run's first call isn't a summary: system %q, %d tools", reqs[3].System, len(reqs[3].Tools))
	}
	if !strings.Contains(lastAsk(reqs[3]), "refund up to 50 € each") || !strings.Contains(lastAsk(reqs[3]), "Owner: b") {
		t.Fatalf("the summary wasn't asked of the first two runs: %q", lastAsk(reqs[3]))
	}
	got := texts(reqs[4].Messages)
	want := []string{"user: " + runner.SummaryNote + "\n\nSUMMARY of one and two", "user: " + runner.OpeningNote + long("refund up to 50 € each, 100 € a day"),
		"user: " + long("c"), "assistant: three", "user: " + long("d")}
	if !slices.Equal(got, want) {
		t.Fatalf("the run after the summary saw\n%q\nwant\n%q", got, want)
	}
	_, events, err := f.store.ReadRun(ctx, "t1", r.Session, runs[3].ID)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(events, func(e session.Event) bool { return e.Kind == session.Summary })
	if i < 0 {
		t.Fatal("the summary wasn't recorded")
	}
	var s session.SummaryBody
	_ = json.Unmarshal(events[i].Body, &s)
	if s.Through != runs[1].ID || s.Runs != 2 || s.Opening != long("refund up to 50 € each, 100 € a day") {
		t.Fatalf("the summary covers through %q (%d runs), opening %q; want the second run, 2", s.Through, s.Runs, s.Opening)
	}
	if got, want := f.get(runs[3].ID).Cost, int64(2*(3000+1500)); got != want {
		t.Fatalf("the run costs %d, want the summary's cost counted too: %d", got, want)
	}

	// The next run starts from the summary, and doesn't summarise again.
	f.execute(f.start(server.Start{Session: r.Session, Input: long("e")}))
	reqs = f.model.requests()
	if len(reqs) != 6 || reqs[5].System == agent.SummarySystem {
		t.Fatalf("%d calls; the fifth run summarised again", len(reqs))
	}
	if got := texts(reqs[5].Messages); got[0] != "user: "+runner.SummaryNote+"\n\nSUMMARY of one and two" || !slices.Contains(got, "assistant: four") {
		t.Fatalf("the fifth run saw %q", got)
	}
}

func TestASummaryThatFailsSaysTheEarlierPartIsMissing(t *testing.T) {
	long := func(s string) string { return s + strings.Repeat(" and more", 20) }
	f := newFixture(t, say("one"), say("two"), model.Reply{Stop: model.Refused}, say("three"))
	d := registrytest.Definition("t1", "rae")
	d.History, d.HistoryTokens = 50, 60
	if err := f.agents.Put(ctx, d); err != nil {
		t.Fatal(err)
	}
	r := f.execute(f.start(server.Start{Input: long("a")}))
	f.execute(f.start(server.Start{Session: r.Session, Input: long("b")}))
	f.execute(f.start(server.Start{Session: r.Session, Input: long("c")}))
	got := texts(f.model.requests()[3].Messages)
	if got[0] != "user: "+runner.DroppedNote || got[1] != "user: "+runner.OpeningNote+long("a") {
		t.Fatalf("after a failed summary, the run saw %q", got)
	}
}
