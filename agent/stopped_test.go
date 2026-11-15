package agent_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
)

// resultOf is the result the model read for a call, from the model's requests.
func resultOf(reqs []model.Request, callID string) (string, bool) {
	for _, req := range reqs {
		for _, m := range req.Messages {
			for _, b := range m.Blocks {
				if b.Kind == model.ToolResult && b.CallID == callID {
					return b.Result, true
				}
			}
		}
	}
	return "", false
}

// The Sheets taf (13 Nov): its search of the spreadsheets was refused for
// want of a scope, and it made a new sheet of the same name. Now, while a
// read of the run is refused, nothing is created or changed in it: the
// create goes back unmade, with words to stop on, and reaches nobody.
// Category: authority
// Attack: a refused search, then a create, a write, and the search again
// Refused: the create and the write, unmade; a search that answers lifts it
func TestARefusedReadNeverLeadsToACreate(t *testing.T) {
	search := &seqTool{name: "sheets__search", access: tool.Read, results: []tool.Result{
		{Text: "Google Sheets refused it (it answered 403): insufficient authentication scopes", IsError: true},
		{Text: `{"files":[{"id":"1dS8"}]}`},
	}}
	create := &seqTool{name: "sheets__create", access: tool.Write, results: []tool.Result{{Text: `{"spreadsheetId":"2xYz"}`, State: "allowed"}}}
	write := &seqTool{name: "sheets__write", access: tool.Write, results: []tool.Result{{Text: `{"updatedCells":2}`, State: "allowed"}}}
	// The search named the sheet, and so do the create and the write: what the
	// failed read asked about is what they'd change.
	m := &script{replies: []model.Reply{
		use("c1", "sheets__search", `{"query":"Follow-up leads"}`),
		use("c2", "sheets__create", `{"title":"Follow-up leads"}`),
		use("c3", "sheets__write", `{"sheet":"Follow-up leads","range":"D2:D3"}`),
		use("c4", "sheets__search", `{"query":"Follow-up leads"}`),                // its one more try answers
		use("c5", "sheets__write", `{"sheet":"Follow-up leads","range":"D2:D3"}`), // now it knows: made
		say("Done: filled in Next step on Follow-up leads."),
	}}
	store, r := start(t, session.KindRun)
	ctx := context.Background()
	if _, _, err := agent.Run(ctx, newAgent(m, search, create, write), r, nil, "Fill in Next step.", nil); err != nil {
		t.Fatal(err)
	}
	if create.n != 0 {
		t.Fatalf("the create reached the app %d time(s) after a refused read", create.n)
	}
	for _, id := range []string{"c2", "c3"} {
		if got, _ := resultOf(m.reqs, id); got != agent.AfterRefusedRead {
			t.Errorf("%s came back %q, want it unmade", id, got)
		}
	}
	if write.n != 1 {
		t.Fatalf("the write reached the app %d time(s), want once, after the search answered", write.n)
	}
	// The record says why each wasn't made, and counts them nowhere.
	if err := r.End(ctx); err != nil {
		t.Fatal(err)
	}
	_, events, _ := store.Read(ctx, "t1", r.Session().ID)
	stopped := 0
	for _, e := range events {
		var b agent.CalledBody
		if e.Kind == session.Called && json.Unmarshal(e.Body, &b) == nil && b.Outcome == agent.OutcomeStopped {
			stopped++
			if agent.TallyOf(b.Outcome) != "" {
				t.Errorf("a stopped call is counted as %q", agent.TallyOf(b.Outcome))
			}
		}
	}
	if stopped != 2 {
		t.Fatalf("%d calls recorded as stopped, want 2", stopped)
	}
}

// A tool gets one more try once it failed, to find out why. Failing twice in
// a run, it isn't called again in it; the run's other tools still are.
// Category: loop
// Attack: the same refused call sent again and again
// Refused: the third, unmade
func TestAToolThatFailedTwiceIsNotTriedAgain(t *testing.T) {
	post := &seqTool{name: "linkedin__post", access: tool.Write, results: []tool.Result{
		{Text: "LinkedIn refused it (it answered 403)", IsError: true},
	}}
	read := &seqTool{name: "github__releases", access: tool.Read, results: []tool.Result{{Text: `[{"tag":"v2"}]`}}}
	// The same post each time: an app's error counts per call, the tool with its input.
	m := &script{replies: []model.Reply{
		use("c1", "linkedin__post", `{"text":"v2 is out"}`),
		use("c2", "linkedin__post", `{"text":"v2 is out"}`),
		use("c3", "linkedin__post", `{"text":"v2 is out"}`),
		uses("c4", "github__releases"),
		say("LinkedIn refused the post: the connection can't post as you. Sign in to LinkedIn again from Account, then ask me to post."),
	}}
	_, r := start(t, session.KindRun)
	if _, _, err := agent.Run(context.Background(), newAgent(m, post, read), r, nil, "Post the changelog.", nil); err != nil {
		t.Fatal(err)
	}
	if post.n != 2 {
		t.Fatalf("the post reached the app %d times, want 2 (one try, one more)", post.n)
	}
	if got, _ := resultOf(m.reqs, "c3"); got != agent.AfterTwoFailures {
		t.Fatalf("the third post came back %q", got)
	}
	if read.n != 1 {
		t.Fatalf("another tool wasn't called: %d", read.n)
	}
}

// A refused act isn't a refused read: the run may still read, and act again
// once (its one more try). Held for the owner isn't refused at all.
func TestARefusedActOrAHoldDoesntStopReads(t *testing.T) {
	refund := &seqTool{name: "stripe__refund", access: tool.Write, results: []tool.Result{
		{Text: "over the Card's limit", IsError: true, State: tool.StateRefused},
		{Text: "held for the owner", State: tool.StateHeld},
	}}
	get := &seqTool{name: "stripe__charge", access: tool.Read, results: []tool.Result{{Text: `{"amount":700}`}}}
	m := &script{replies: []model.Reply{
		uses("c1", "stripe__refund"),
		uses("c2", "stripe__charge"),
		say("The refund is over the Card's limit."),
	}}
	_, r := start(t, session.KindRun)
	if _, _, err := agent.Run(context.Background(), newAgent(m, refund, get), r, nil, "Refund ch_1.", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := resultOf(m.reqs, "c2"); got != `{"amount":700}` {
		t.Fatalf("the read after a refused act came back %q", got)
	}
}

// The agent's own tools (its notes, its cards, planque's requests) aren't
// calls to an app: a refusal of one never stops the run's acts.
func TestTheAgentsOwnToolsDontCount(t *testing.T) {
	own := &ownTool{name: "planque", result: tool.Result{Text: "refused: the request names no tools", IsError: true}}
	create := &seqTool{name: "sheets__create", access: tool.Write, results: []tool.Result{{Text: `{"spreadsheetId":"2xYz"}`, State: "allowed"}}}
	m := &script{replies: []model.Reply{
		uses("c1", "planque"),
		uses("c2", "planque"),
		uses("c3", "planque"),
		uses("c4", "sheets__create"),
		say("Made."),
	}}
	_, r := start(t, session.KindRun)
	if _, _, err := agent.Run(context.Background(), newAgent(m, own, create), r, nil, "Set it up.", nil); err != nil {
		t.Fatal(err)
	}
	if own.n != 3 || create.n != 1 {
		t.Fatalf("own tool called %d times, create %d", own.n, create.n)
	}
}

// ownTool is one of the agent's own tools, always answering result.
type ownTool struct {
	name   string
	result tool.Result
	n      int
}

func (o *ownTool) Spec() model.ToolSpec {
	return model.ToolSpec{Name: o.name, InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (o *ownTool) Access() tool.Access { return tool.Own }
func (o *ownTool) Gateway() bool       { return false }
func (o *ownTool) Call(context.Context, json.RawMessage) (tool.Result, error) {
	o.n++
	return o.result, nil
}

// A run continued after a hold remembers what failed before it: a tool that
// failed once before the pause and once after isn't tried a third time.
func TestAContinuedRunRemembersItsFailures(t *testing.T) {
	store := memory.New()
	rec, err := session.NewRecorder(store, session.DefaultPolicy(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	post := &seqTool{name: "linkedin__post", access: tool.Write, results: []tool.Result{{Text: "LinkedIn refused it (it answered 403)", IsError: true}}}
	m := &script{replies: []model.Reply{
		use("c1", "linkedin__post", `{}`),
		use("c2", "stripe_refund", `{"charge":"ch_1"}`), // held: the run pauses
		say("Waiting for you."),
		use("c3", "linkedin__post", `{}`),
		use("c4", "linkedin__post", `{}`),
		say("LinkedIn refused the post twice."),
	}}
	a := newAgent(m, post, heldRefund())
	stored, s := paused(t, store, rec, a)
	ctx := context.Background()
	r, err := rec.Resume(ctx, s, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := agent.Continue(ctx, a, r, nil, stored, approved(), nil); err != nil {
		t.Fatal(err)
	}
	if post.n != 2 {
		t.Fatalf("the post reached the app %d times across the pause, want 2", post.n)
	}
	if got, _ := resultOf(m.reqs, "c4"); got != agent.AfterTwoFailures {
		t.Fatalf("the third post came back %q", got)
	}
}

// The chargebacks taf (13 Nov): its Card leaves out the shop's order lookup,
// so KIFF refuses it, and the taf still settles the disputes its Card
// covers. A read refused outside the Card is the owner's limit, not a sign
// of what the app holds: it stops no write. A read that failed still does.
// Category: authority
// Attack: none; the regression was a run that stopped doing its job
// Refused: the order lookups, by KIFF; the dispute writes are made
func TestAReadRefusedOnTheCardStopsNoWrite(t *testing.T) {
	order := &seqTool{name: "shop_get_order", access: tool.Read, results: []tool.Result{
		{Text: "refused: no Card of this agent allows shop_get_order", IsError: true, State: tool.StateRefused},
	}}
	evidence := &seqTool{name: "update_dispute", access: tool.Write, results: []tool.Result{{Text: `{"id":"du_1","status":"under_review"}`, State: tool.StateForwarded}}}
	accept := &seqTool{name: "close_dispute", access: tool.Write, results: []tool.Result{{Text: `{"id":"du_2","status":"lost"}`, State: tool.StateForwarded}}}
	m := &script{replies: []model.Reply{
		use("c1", "shop_get_order", `{"order":"T-2001"}`),
		use("c2", "update_dispute", `{"dispute":"du_1"}`),
		use("c3", "close_dispute", `{"dispute":"du_2"}`),
		say("Evidence went on T-2001; T-2002 is accepted."),
	}}
	_, r := start(t, session.KindRun)
	if _, _, err := agent.Run(context.Background(), newAgent(m, order, evidence, accept), r, nil, "Work the open disputes.", nil); err != nil {
		t.Fatal(err)
	}
	if evidence.n != 1 || accept.n != 1 {
		t.Fatalf("after a read refused on the Card: update_dispute made %d, close_dispute %d; want 1 each", evidence.n, accept.n)
	}
	// Refused twice on the Card, the lookup isn't tried a third time: that rule holds for every refusal.
	order2 := &seqTool{name: "shop_get_order", access: tool.Read, results: []tool.Result{
		{Text: "refused", IsError: true, State: tool.StateRefused}, {Text: "refused", IsError: true, State: tool.StateRefused},
	}}
	m2 := &script{replies: []model.Reply{
		use("c1", "shop_get_order", `{"order":"T-2001"}`),
		use("c2", "shop_get_order", `{"order":"T-2002"}`),
		use("c3", "shop_get_order", `{"order":"T-2003"}`),
		say("The shop's orders are outside the Card."),
	}}
	_, r2 := start(t, session.KindRun)
	if _, _, err := agent.Run(context.Background(), newAgent(m2, order2), r2, nil, "Work the open disputes.", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := resultOf(m2.reqs, "c3"); got != agent.AfterTwoRefusals || order2.n != 2 {
		t.Fatalf("the third lookup came back %q after %d calls", got, order2.n)
	}
}

// A run continued after a hold still knows a read was refused on the Card,
// not failed: the state is kept with the run's messages, so the run's other
// work goes on after the pause too. (A read that failed before a pause can't
// be followed by the act that would pause it: that act comes back unmade.)
func TestAContinuedRunStillKnowsACardRefusal(t *testing.T) {
	store := memory.New()
	rec, err := session.NewRecorder(store, session.DefaultPolicy(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	order := &seqTool{name: "shop_get_order", access: tool.Read, results: []tool.Result{{Text: "refused: outside the Card", IsError: true, State: tool.StateRefused}}}
	accept := &seqTool{name: "close_dispute", access: tool.Write, results: []tool.Result{{Text: `{"id":"du_2","status":"lost"}`, State: tool.StateForwarded}}}
	m := &script{replies: []model.Reply{
		use("c1", "shop_get_order", `{"order":"T-2002"}`),
		use("c2", "stripe_refund", `{"charge":"ch_1"}`), // held: the run pauses
		say("Waiting for you."),
		use("c3", "close_dispute", `{"dispute":"du_2"}`),
		say("Done."),
	}}
	a := newAgent(m, order, accept, heldRefund())
	stored, s := paused(t, store, rec, a)
	ctx := context.Background()
	r, err := rec.Resume(ctx, s, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := agent.Continue(ctx, a, r, nil, stored, approved(), nil); err != nil {
		t.Fatal(err)
	}
	if accept.n != 1 {
		t.Fatalf("close_dispute reached the app %d time(s) after the pause, want 1", accept.n)
	}
}

// Eval 43695849959's handoff-receiver: two reads of a dispute with mistyped
// ids failed, and the evidence on the right dispute was never submitted. A
// failed read stops only a write on what it asked about: another item's
// write goes; the same item's, or any write after a read that named nothing
// (a search or list of everything), still waits until a read answers.
// Category: authority
// Attack: a read of a wrong id fails, then writes on the right id and on the wrong one
// Refused: the write on the wrong id; after an unscoped read that failed, every write
func TestAFailedReadStopsOnlyWritesOnWhatItAskedAbout(t *testing.T) {
	read := &seqTool{name: "stripe__retrieve_dispute", access: tool.Read, results: []tool.Result{
		{Text: "Stripe answered 404: No such dispute: 'dp_2O03'", IsError: true},
	}}
	evidence := &seqTool{name: "stripe__update_dispute", access: tool.Write, results: []tool.Result{
		{Text: `{"id":"dp_2003","status":"under_review"}`, State: tool.StateForwarded},
		{Text: `{"id":"dp_2O03"}`, State: tool.StateForwarded},
	}}
	m := &script{replies: []model.Reply{
		use("c1", "stripe__retrieve_dispute", `{"dispute":"dp_2O03"}`),
		use("c2", "stripe__update_dispute", `{"dispute":"dp_2003","evidence":{"uncategorized_text":"Signed delivery"},"submit":true}`),
		use("c3", "stripe__update_dispute", `{"dispute":"DP_2O03","submit":true}`),
		say("Evidence went on T-2003."),
	}}
	_, r := start(t, session.KindRun)
	if _, _, err := agent.Run(context.Background(), newAgent(m, read, evidence), r, nil, "Work what was handed.", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := resultOf(m.reqs, "c2"); got == agent.AfterRefusedRead || evidence.n != 1 {
		t.Fatalf("the right dispute's evidence came back %q, %d write(s) made; want it made", got, evidence.n)
	}
	// The same id the failed read named, in any case: unknown, so unmade.
	if got, _ := resultOf(m.reqs, "c3"); got != agent.AfterRefusedRead {
		t.Fatalf("a write on the id the failed read named came back %q, want it unmade", got)
	}

	// A read that named nothing (a list of every dispute) failed: no write
	// goes, whatever it names, and a number in it doesn't count as a name.
	list := &seqTool{name: "stripe__list_disputes", access: tool.Read, results: []tool.Result{
		{Text: "Stripe answered 500", IsError: true},
	}}
	evidence2 := &seqTool{name: "stripe__update_dispute", access: tool.Write, results: []tool.Result{{Text: `{}`, State: tool.StateForwarded}}}
	m2 := &script{replies: []model.Reply{
		use("c1", "stripe__list_disputes", `{"limit":20,"expand":true}`),
		use("c2", "stripe__update_dispute", `{"dispute":"dp_2003","submit":true}`),
		say("Stripe's list failed."),
	}}
	_, r2 := start(t, session.KindRun)
	if _, _, err := agent.Run(context.Background(), newAgent(m2, list, evidence2), r2, nil, "Work the open disputes.", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := resultOf(m2.reqs, "c2"); got != agent.AfterRefusedRead || evidence2.n != 0 {
		t.Fatalf("after an unscoped read failed, a write came back %q, made %d time(s)", got, evidence2.n)
	}
}

// dev-android on #502: a failed list with a filter names no item, and a
// failed read of a numeric id names that id. Only keys that name an item
// count (an id, or a word of the tool's name), numbers as their text.
// Category: authority
// Attack: a filtered list fails, then a write elsewhere; a numeric id's read fails, then a write on it
// Refused: both writes, unmade; a write on another order goes
func TestAFailedFilteredListOrNumericIDStillStopsTheWrite(t *testing.T) {
	cases := []struct {
		name, readTool, read, write string
		stopped                     bool
	}{
		{"a filtered list with a cursor", "stripe__list_disputes", `{"status":"needs_response","starting_after":"dp_1","limit":20}`, `{"dispute":"dp_2003","submit":true}`, true},
		{"a numeric id", "shop__get_order", `{"order_id":12345,"fields":"items"}`, `{"order_id":12345,"status":"cancelled"}`, true},
		{"a camel-case id", "shop__get_order", `{"orderId":"A-77"}`, `{"order":{"orderId":"a-77"},"status":"cancelled"}`, true},
		{"another order", "shop__get_order", `{"order_id":12345}`, `{"order_id":12346,"status":"cancelled"}`, false},
		{"another order, by a camel-case id", "shop__get_order", `{"orderId":"A-77"}`, `{"orderId":"B-1","status":"cancelled"}`, false},
		{"a key that only ends in id", "shop__list_orders", `{"paid":"yes","valid":"now"}`, `{"order_id":9}`, true},
	}
	for _, c := range cases {
		read := &seqTool{name: c.readTool, access: tool.Read, results: []tool.Result{{Text: "the app answered 500", IsError: true}}}
		write := &seqTool{name: "app__update", access: tool.Write, results: []tool.Result{{Text: `{}`, State: tool.StateForwarded}}}
		m := &script{replies: []model.Reply{use("c1", c.readTool, c.read), use("c2", "app__update", c.write), say("Done.")}}
		_, r := start(t, session.KindRun)
		if _, _, err := agent.Run(context.Background(), newAgent(m, read, write), r, nil, "Work it.", nil); err != nil {
			t.Fatal(err)
		}
		got, _ := resultOf(m.reqs, "c2")
		if stopped := got == agent.AfterRefusedRead; stopped != c.stopped || (write.n == 0) != c.stopped {
			t.Errorf("%s: the write came back %q, made %d time(s); want stopped %v", c.name, got, write.n, c.stopped)
		}
	}
}
