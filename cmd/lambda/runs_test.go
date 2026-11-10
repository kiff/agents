package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/registry/registrytest"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/runner"
	"github.com/kiff/agents/schedule"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/store/memory"
)

// replies is a model that answers each turn with the next reply, and counts
// its turns.
type replies struct {
	mu    sync.Mutex
	next  []model.Reply
	turns int
}

func (r *replies) Turn(context.Context, model.Request, func(string)) (model.Reply, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turns++
	if len(r.next) == 0 {
		return model.Reply{}, errors.New("no reply left")
	}
	next := r.next[0]
	r.next = r.next[1:]
	return next, nil
}

func answer(text string) model.Reply {
	return model.Reply{Blocks: []model.Block{{Kind: model.Text, Text: text}}, Stop: model.EndTurn}
}

type keys struct{}

func (keys) Key(context.Context, string, string) (string, error) { return "kiff-agent-key", nil }

// invocations records each Invoke; err and status say how it answers.
type invocations struct {
	got    []*lambdasvc.InvokeInput
	err    error
	status int32
}

func (i *invocations) Invoke(_ context.Context, in *lambdasvc.InvokeInput, _ ...func(*lambdasvc.Options)) (*lambdasvc.InvokeOutput, error) {
	i.got = append(i.got, in)
	if i.err != nil {
		return nil, i.err
	}
	status := i.status
	if status == 0 {
		status = 202
	}
	return &lambdasvc.InvokeOutput{StatusCode: status}, nil
}

// startRun posts a run of agent through the function URL, as tafs cloud does.
func startRun(t *testing.T, f *function, agent, body string) (int, string) {
	t.Helper()
	e := events.LambdaFunctionURLRequest{Version: "2.0", RawPath: "/agents/" + agent + "/runs", Body: body,
		Headers: map[string]string{"kiff-tenant": "t1", "content-type": "application/json"},
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "POST"},
			Authorizer: &events.LambdaFunctionURLRequestContextAuthorizerDescription{
				IAM: &events.LambdaFunctionURLRequestContextAuthorizerIAMDescription{UserARN: role + "fn"}}}}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.handle(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	resp := out.(*events.LambdaFunctionURLStreamingResponse)
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestARunStreamsOnLambda(t *testing.T) {
	m := &replies{next: []model.Reply{answer("Order 42 is refunded.")}}
	f, _, agents, _ := newFunctionWith(t, m, &invocations{})
	_ = agents.Put(ctx, registrytest.Definition("t1", "rae"))
	status, body := startRun(t, f, "rae", `{"input":"Refund order 42."}`)
	if status != 200 || !strings.Contains(body, "event: end") || !strings.Contains(body, `"status":"COMPLETED"`) ||
		!strings.Contains(body, "Order 42 is refunded.") {
		t.Fatalf("got %d\n%s", status, body)
	}
}

func TestABackgroundRunIsExecutedByAnotherInvocation(t *testing.T) {
	m := &replies{next: []model.Reply{answer("Order 42 is refunded.")}}
	inv := &invocations{}
	f, _, agents, runs := newFunctionWith(t, m, inv)
	_ = agents.Put(ctx, registrytest.Definition("t1", "rae"))

	status, body := startRun(t, f, "rae", `{"input":"Refund order 42.","background":true}`)
	if status != 202 {
		t.Fatalf("got %d %s", status, body)
	}
	var started run.Run
	if err := json.Unmarshal([]byte(body), &started); err != nil || started.Status != run.Pending {
		t.Fatalf("got %s (%v)", body, err)
	}
	if m.turns != 0 {
		t.Fatal("the request ran the model itself")
	}

	// One asynchronous invoke of this function, naming the run and nothing else.
	if len(inv.got) != 1 {
		t.Fatalf("%d invokes", len(inv.got))
	}
	in := inv.got[0]
	if *in.FunctionName != "agents-fn" || in.InvocationType != lambdatypes.InvocationTypeEvent {
		t.Fatalf("invoked %s as %s", *in.FunctionName, in.InvocationType)
	}
	want := `{"run":{"tenant":"t1","id":"` + started.ID + `"}}`
	if string(in.Payload) != want {
		t.Fatalf("payload %s, want %s", in.Payload, want)
	}

	// That invocation executes the run to its stop.
	if _, err := f.handle(ctx, in.Payload); err != nil {
		t.Fatal(err)
	}
	r, err := runs.Get(ctx, "t1", started.ID)
	if err != nil || r.Status != run.Completed || r.Output != "Order 42 is refunded." {
		t.Fatalf("got %+v (%v)", r, err)
	}

	// Lambda delivers at least once: the same event again changes nothing.
	if _, err := f.handle(ctx, in.Payload); err != nil {
		t.Fatalf("a repeated event failed the invocation: %v", err)
	}
	if again, _ := runs.Get(ctx, "t1", started.ID); again.Version != r.Version || m.turns != 1 {
		t.Fatalf("a repeated event ran it again: %d turns, version %d then %d", m.turns, r.Version, again.Version)
	}
}

func TestARunThatCantBeQueuedIsAnError(t *testing.T) {
	for _, inv := range []*invocations{{err: errors.New("throttled")}, {status: 200}} {
		f, _, agents, runs := newFunctionWith(t, &replies{}, inv)
		_ = agents.Put(ctx, registrytest.Definition("t1", "rae"))
		status, body := startRun(t, f, "rae", `{"input":"Refund order 42.","background":true}`)
		if status != 500 {
			t.Fatalf("got %d %s", status, body)
		}
		list, err := runs.List(ctx, "t1", "rae", 10)
		if err != nil || len(list) != 1 || list[0].Status != run.Error {
			t.Fatalf("got %+v (%v): a run no invocation will claim must not wait", list, err)
		}
	}
}

func TestARunEventMustNameARun(t *testing.T) {
	f, _, _, _ := newFunctionWith(t, &replies{}, &invocations{})
	for _, e := range []string{`{"run":{}}`, `{"run":{"tenant":"t1"}}`, `{"run":{"id":"x"}}`, `{"run":null}`, `{"run":"x"}`} {
		if _, err := f.handle(ctx, json.RawMessage(e)); err == nil {
			t.Errorf("%s: accepted", e)
		}
	}
	// A run that doesn't exist is logged, not retried.
	if _, err := f.handle(ctx, json.RawMessage(`{"run":{"tenant":"t1","id":"missing"}}`)); err != nil {
		t.Fatalf("a missing run failed the invocation: %v", err)
	}
}

type secretValues map[string]*string

func (s secretValues) GetSecretValue(_ context.Context, in *sm.GetSecretValueInput, _ ...func(*sm.Options)) (*sm.GetSecretValueOutput, error) {
	v, ok := s[*in.SecretId]
	if !ok {
		return nil, errors.New("ResourceNotFoundException")
	}
	return &sm.GetSecretValueOutput{Name: in.SecretId, SecretString: v}, nil
}

func TestTheModelKeyIsReadWhenATurnNeedsIt(t *testing.T) {
	key, empty := " sk-model \n", "  "
	reads := 0
	var made []string
	values := secretValues{"model/anthropic": &key, "model/empty": &empty, "model/binary": nil}
	keyed := func(name string) *keyedModel {
		return &keyedModel{
			key: func(ctx context.Context) (string, error) { reads++; return modelKey(ctx, values, name) },
			make: func(k string) model.Model {
				made = append(made, k)
				return &replies{next: []model.Reply{answer("one"), answer("two")}}
			},
		}
	}

	k := keyed("model/anthropic")
	if reads != 0 {
		t.Fatal("the key was read before any turn")
	}
	for range 2 {
		if _, err := k.Turn(ctx, model.Request{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 1 || len(made) != 1 || made[0] != "sk-model" {
		t.Fatalf("%d reads, made with %q", reads, made)
	}

	for _, name := range []string{"model/empty", "model/binary", "model/missing"} {
		reads, made = 0, nil
		k := keyed(name)
		for range 2 {
			if _, err := k.Turn(ctx, model.Request{}, nil); err == nil {
				t.Fatalf("%s: a turn ran without a key", name)
			}
		}
		if reads != 2 || len(made) != 0 {
			t.Fatalf("%s: %d reads, %d made: a failed read must be tried again", name, reads, len(made))
		}
	}
}

func TestTheModelKeyIsOutsideTheAgentsKeys(t *testing.T) {
	for secret, ok := range map[string]bool{
		"model/anthropic":         true,
		"agents-model":            true, // shares the text, not the "agents/" prefix
		"agents/models/anthropic": false,
		"agents/":                 false,
		"arn:aws:secretsmanager:eu-west-3:000000000000:secret:agents/models/anthropic-AbCdEf": false,
	} {
		if err := outsidePrefix(secret, "agents/"); (err == nil) != ok {
			t.Errorf("%s: got %v", secret, err)
		}
	}
}

// TestTheSweepEndsWhatNobodyElseWill: a background run whose invocation never
// came, and a run whose invocation crashed mid-run, are both ended by a sweep
// event, with their expiry from the agent's keep rules; a run still in its
// lease is left alone.
func TestTheSweepEndsWhatNobodyElseWill(t *testing.T) {
	inv := &invocations{}
	f, _, agents, runs := newFunctionWith(t, &replies{}, inv)
	d := registrytest.Definition("t1", "rae")
	_ = agents.Put(ctx, d)
	rn := f.runner.(*runner.Runner)
	start := func() run.Run {
		r, err := rn.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "Refund order 42."})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	now := time.Now().UTC()

	unclaimed := start()
	crashed := start()
	if _, err := run.Claim(ctx, runs, "t1", crashed.ID, "c1", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if err := rn.Sessions.Begin(ctx, "t1", crashed.Session, crashed.ID, crashed.Created); err != nil {
		t.Fatal(err)
	}
	// Past PendingTimeout and the crashed run's lease; the healthy one's invocation holds a live lease (claimed a
	// second ago here; renewed on its heartbeat in life).
	later := now.Add(run.PendingTimeout + run.LeaseGrace + 2*time.Minute)
	healthy := start()
	if _, err := run.Claim(ctx, runs, "t1", healthy.ID, "c2", later.Add(time.Hour), later.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	f.now = func() time.Time { return later }
	if _, err := f.handle(ctx, json.RawMessage(`{"sweep":{}}`)); err != nil {
		t.Fatal(err)
	}

	// The crashed run stopped before any turn acted: restarted from its start by the sweep, its recording held.
	if got, err := runs.Get(ctx, "t1", crashed.ID); err != nil || got.Status != run.Paused || got.Resumes != 1 || !got.Retrying() {
		t.Fatalf("the crashed run: %+v (%v), want PAUSED, restarting", got, err)
	}
	// The unclaimed run's dispatch was lost: it is sent to an invocation again, not ended.
	if got, err := runs.Get(ctx, "t1", unclaimed.ID); err != nil || got.Status != run.Pending || got.Resumes != 1 || !got.Wait.Equal(later.Add(run.PendingTimeout)) {
		t.Fatalf("the unclaimed run: %+v (%v), want PENDING, dispatched again", got, err)
	}
	if n := len(inv.got); n != 1 {
		t.Fatalf("%d invocations, want the unclaimed run's one", n)
	}
	if st, err := rn.Sessions.State(ctx, "t1", crashed.Session, crashed.ID); err != nil || !st.Held || st.Ended {
		t.Fatalf("the crashed run's recording: %+v (%v), want held for its restart", st, err)
	}
	if got, _ := runs.Get(ctx, "t1", healthy.ID); got.Status != run.Running {
		t.Fatalf("a run still in its lease was swept: %s", got.Status)
	}
}

// A paused run whose held calls are all answered (cmd/kiffalerts records the
// answers) is sent to another invocation by the sweep, as a background run
// is; one still waiting on KIFF is not.
func TestTheSweepContinuesAResolvedPausedRun(t *testing.T) {
	inv := &invocations{}
	f, _, agents, runs := newFunctionWith(t, &replies{}, inv)
	_ = agents.Put(ctx, registrytest.Definition("t1", "rae"))
	rn := f.runner.(*runner.Runner)
	now := time.Now().UTC()
	paused := func(answered time.Time) run.Run {
		r, err := rn.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "Refund order 42."})
		if err != nil {
			t.Fatal(err)
		}
		if r, err = run.Claim(ctx, runs, "t1", r.ID, "c1", now.Add(time.Minute), now); err != nil {
			t.Fatal(err)
		}
		r, err = run.Save(ctx, runs, r, now, func(n *run.Run) error {
			n.Status = run.Paused
			n.Requirements = []run.Requirement{{CallID: "c1", Tool: "stripe_refund", Operation: "op-1", Exception: "exc-" + r.ID,
				Answered: answered}}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	resolved, waiting := paused(now), paused(time.Time{})

	f.now = func() time.Time { return now.Add(run.ResolvedGrace + time.Second) }
	if _, err := f.handle(ctx, json.RawMessage(`{"sweep":{}}`)); err != nil {
		t.Fatal(err)
	}
	if len(inv.got) != 1 || aws.ToString(inv.got[0].FunctionName) != "agents-fn" || inv.got[0].InvocationType != lambdatypes.InvocationTypeEvent {
		t.Fatalf("invocations %+v, want the resolved run sent to the background once", inv.got)
	}
	var e runEvent
	if err := json.Unmarshal(inv.got[0].Payload, &e); err != nil || e.Run.Tenant != "t1" || e.Run.ID != resolved.ID {
		t.Fatalf("sent %s, want %s's run event", inv.got[0].Payload, resolved.ID)
	}
	if got, _ := runs.Get(ctx, "t1", waiting.ID); got.Status != run.Paused || got.Version != waiting.Version {
		t.Fatalf("a run still waiting on KIFF was touched: %+v", got)
	}
}

// failingRuns is a run store whose Due fails.
type failingRuns struct{ run.Runs }

func (failingRuns) Due(context.Context, time.Time, int) ([]run.Run, error) {
	return nil, errors.New("throttled")
}

func TestAFailedSweepShows(t *testing.T) {
	f, _, _, runs := newFunctionWith(t, &replies{}, &invocations{})
	f.sweeper = sweeping(failingRuns{runs}, f.runner.(*runner.Runner), nil)
	if _, err := f.handle(ctx, json.RawMessage(`{"sweep":{}}`)); err == nil {
		t.Fatal("a sweep that read nothing succeeded")
	}
}

// TestARepeatedFiringRunsWhatTheFirstLeftPending: the first delivery stored
// the run and died before claiming it; the next delivery executes it.
func TestARepeatedFiringRunsWhatTheFirstLeftPending(t *testing.T) {
	m := &replies{next: []model.Reply{answer("Payouts are reconciled.")}}
	f, _, agents, runs := newFunctionWith(t, m, &invocations{})
	_ = agents.Put(ctx, registrytest.Definition("t1", "payouts"))
	fi := schedule.Firing{Tenant: "t1", Schedule: "morning", Agent: "payouts", Input: "Reconcile payouts.",
		Due: time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)}
	if _, err := f.runner.Start(ctx, server.Start{Tenant: "t1", Agent: "payouts", Trigger: run.BySchedule,
		Source: "morning", Input: fi.Input, ID: fi.Key()}); err != nil {
		t.Fatal(err)
	}
	if err := f.fire(ctx, fi); err != nil {
		t.Fatal(err)
	}
	if r, err := runs.Get(ctx, "t1", fi.Key()); err != nil || r.Status != run.Completed {
		t.Fatalf("got %+v (%v)", r, err)
	}
}

// A firing whose run the model refused at its first call (no reply left
// here, for want of credits there) did nothing: fire says so, and so does a
// repeated delivery, so the schedule uses up nothing it handed over. A run
// that acted before it failed is just a run.
func TestAFiringWhoseRunDidNothingSaysSo(t *testing.T) {
	f, _, agents, runs := newFunctionWith(t, &replies{}, &invocations{})
	_ = agents.Put(ctx, registrytest.Definition("t1", "payouts"))
	fi := schedule.Firing{Tenant: "t1", Schedule: "inbox", Agent: "payouts", Input: "Answer the new refund requests.",
		Due: time.Date(2026, 11, 6, 13, 30, 0, 0, time.UTC)}
	if err := f.fire(ctx, fi); !errors.Is(err, schedule.ErrUnworked) {
		t.Fatalf("fire = %v, want ErrUnworked", err)
	}
	r, err := runs.Get(ctx, "t1", fi.Key())
	if err != nil || r.Status != run.Error || r.Turns != 0 {
		t.Fatalf("got %+v (%v)", r, err)
	}
	if err := f.fire(ctx, fi); !errors.Is(err, schedule.ErrUnworked) {
		t.Fatalf("a repeated delivery = %v, want ErrUnworked", err)
	}
	change := func(c func(*run.Run)) {
		t.Helper()
		cur, err := runs.Get(ctx, "t1", fi.Key())
		if err != nil {
			t.Fatal(err)
		}
		next := cur
		c(&next)
		next.Version = cur.Version + 1
		if err := runs.Put(ctx, cur, next); err != nil {
			t.Fatal(err)
		}
	}
	// Carried on by the sweep, then ended by it: its invocation may have died
	// after acting, before its turns were stored. A late repeated delivery
	// doesn't hand its items to another run.
	change(func(n *run.Run) { n.Resumes, n.Error = 1, "its lease ran out: the invocation crashed or timed out" })
	if err := f.fire(ctx, fi); !errors.Is(err, schedule.ErrNotAgain) {
		t.Fatalf("a run the sweep ended, delivered again = %v, want ErrNotAgain", err)
	}
	// It worked, then failed, with a write that went through: never handed again either.
	change(func(n *run.Run) {
		n.Resumes, n.Error, n.Turns, n.Sent = 0, runner.FailedRun, 1, 1
		n.Done = []run.Done{{CallID: "c1", Tool: "stripe_refund"}}
	})
	if err := f.fire(ctx, fi); !errors.Is(err, schedule.ErrNotAgain) {
		t.Fatalf("a run that acted, delivered again = %v, want ErrNotAgain", err)
	}
	// It worked, then failed, and changed nothing: its items can go again.
	change(func(n *run.Run) { n.Sent, n.Done = 0, nil })
	if err := f.fire(ctx, fi); !errors.Is(err, schedule.ErrUnfinished) {
		t.Fatalf("a run that only read, delivered again = %v, want ErrUnfinished", err)
	}
	// Still at work, or paused for its owner: it holds what it was handed.
	for _, st := range []run.Status{run.Running, run.Paused} {
		change(func(n *run.Run) { n.Status, n.Error = st, "" })
		if err := f.fire(ctx, fi); err != nil {
			t.Fatalf("a %s run, delivered again = %v, want nil", st, err)
		}
	}
}

// What a firing's run that stopped means for the firing. It didn't finish
// when it failed after working or its turn or cost cap cut it short. What it
// was handed goes to another run only when nothing it did can have changed
// anything: no call that acts may have reached the vendor (Sent, from KIFF's
// word on each call), none went through, the sweep never carried it on, and
// its failure is the runner's own. Anything else that didn't finish is never
// handed again, so nothing is paid or refunded twice. A run cut off by its
// reply running out of room didn't finish either, and one whose turn was
// declined is never handed again: a new run would only be declined too.
func TestWhatAFiringsEndedRunMeans(t *testing.T) {
	failed := run.Run{Status: run.Error, Error: runner.FailedRun, Turns: 3}
	capped := run.Run{Status: run.Completed, Stop: "turn_cap", Turns: 50}
	with := func(r run.Run, c func(*run.Run)) run.Run { c(&r); return r }
	for _, c := range []struct {
		name string
		r    run.Run
		want error
	}{
		{"failed before any turn", run.Run{Status: run.Error, Error: runner.FailedRun}, schedule.ErrUnworked},
		{"stopped by a switch before it began", run.Run{Status: run.Error, Error: runner.FailedSwitching}, schedule.ErrSwitching},
		{"failed after reads only", failed, schedule.ErrUnfinished},
		{"failed after a write KIFF held, refused or failed (not sent)", with(failed, func(r *run.Run) { r.Refused, r.Declined = 2, 1 }), schedule.ErrUnfinished},
		{"failed after a write that may have been sent (its answer lost)", with(failed, func(r *run.Run) { r.Sent = 1 }), schedule.ErrNotAgain},
		{"failed after a write that went through", with(failed, func(r *run.Run) { r.Sent, r.Done = 1, []run.Done{{CallID: "c1", Tool: "stripe_refund"}} }), schedule.ErrNotAgain},
		{"failed with a done call kept by an older runtime (no count)", with(failed, func(r *run.Run) { r.Done = []run.Done{{CallID: "c1", Tool: "stripe_refund"}} }), schedule.ErrNotAgain},
		{"failed after the sweep carried it on", with(failed, func(r *run.Run) { r.Resumes = 1 }), schedule.ErrNotAgain},
		{"ended by the sweep, its turns maybe behind", run.Run{Status: run.Error, Error: "its lease ran out: the invocation crashed or timed out"}, schedule.ErrNotAgain},
		{"cut short by its turn cap after reads only", capped, schedule.ErrUnfinished},
		{"cut short by its cost cap after reads only", with(capped, func(r *run.Run) { r.Stop = "cost_cap" }), schedule.ErrUnfinished},
		{"cut short by its turn cap after a write that went through", with(capped, func(r *run.Run) { r.Sent, r.Done = 1, []run.Done{{CallID: "c1", Tool: "stripe_refund"}} }), schedule.ErrNotAgain},
		{"cut short by its cost cap after a write whose answer was lost", with(capped, func(r *run.Run) { r.Stop, r.Sent = "cost_cap", 1 }), schedule.ErrNotAgain},
		{"answered", run.Run{Status: run.Completed, Stop: "answered", Turns: 4}, nil},
		{"answered after writes", run.Run{Status: run.Completed, Stop: "answered", Turns: 4, Sent: 2}, nil},
		{"out of room mid-answer after reads only", run.Run{Status: run.Completed, Stop: "max_token", Turns: 4}, schedule.ErrUnfinished},
		{"out of room mid-answer after a write that may have been sent", run.Run{Status: run.Completed, Stop: "max_token", Turns: 4, Sent: 1}, schedule.ErrNotAgain},
		{"a turn declined", run.Run{Status: run.Completed, Stop: "refused", Turns: 1}, schedule.ErrNotAgain},
		{"a turn declined after a write", run.Run{Status: run.Completed, Stop: "refused", Turns: 3, Sent: 1}, schedule.ErrNotAgain},
		{"stopped by its owner", run.Run{Status: run.Cancelled, Stop: "cancelled", Turns: 2}, nil},
		{"paused for its owner, after a write", run.Run{Status: run.Paused, Stop: "paused", Turns: 2, Sent: 1}, nil},
		{"still at work", run.Run{Status: run.Running, Turns: 2}, nil},
		{"not yet claimed", run.Run{Status: run.Pending}, nil},
	} {
		if got := ended(c.r); !errors.Is(got, c.want) || (c.want == nil && got != nil) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// A run is unworked only when the runner itself ended it before any turn and
// the sweep never carried it on. A run the sweep resumed or ended may have
// acted with its turns not stored yet (its invocation died before it could
// store them), so what its firing handed it is never handed again.
func TestOnlyARunTheRunnerEndedBeforeAnyTurnIsUnworked(t *testing.T) {
	for _, c := range []struct {
		name string
		r    run.Run
		want bool
	}{
		{"refused at its first call", run.Run{Status: run.Error, Error: runner.FailedRun}, true},
		{"out of time before a reply", run.Run{Status: run.Error, Error: runner.FailedTime}, true},
		{"refused for our AI balance", run.Run{Status: run.Error, Error: runner.FailedAIUnavailable}, true},
		{"not built", run.Run{Status: run.Error, Error: runner.FailedBuild}, true},
		{"not started", run.Run{Status: run.Error, Error: runner.FailedStart}, true},
		{"its agent switching versions", run.Run{Status: run.Error, Error: runner.FailedSwitching}, true},
		{"a turn done", run.Run{Status: run.Error, Error: runner.FailedRun, Turns: 1}, false},
		{"refused for our AI balance after a turn", run.Run{Status: run.Error, Error: runner.FailedAIUnavailable, Turns: 1}, false},
		{"carried on by the sweep", run.Run{Status: run.Error, Error: runner.FailedRun, Resumes: 1}, false},
		{"its lease ran out", run.Run{Status: run.Error, Error: "its lease ran out: the invocation crashed or timed out"}, false},
		{"stopped as often as it may", run.Run{Status: run.Error, Error: run.NeedsYou, Resumes: run.MaxResumes}, false},
		{"never picked up", run.Run{Status: run.Error, Error: run.NeverStarted, Resumes: run.MaxResumes}, false},
		{"not continued after its held calls", run.Run{Status: run.Error, Error: "it could not continue once KIFF resolved its held calls"}, false},
		{"completed", run.Run{Status: run.Completed}, false},
	} {
		if got := unworked(c.r); got != c.want {
			t.Errorf("%s: unworked = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestARepeatedFiringLeavesAPausedRunToItsAnswer(t *testing.T) {
	f, _, agents, runs := newFunctionWith(t, &replies{}, &invocations{})
	_ = agents.Put(ctx, registrytest.Definition("t1", "payouts"))
	collected := 0
	f.runner.(*runner.Runner).Collect = func(context.Context, agent.Agent, []run.Requirement) ([]agent.ToolResultBody, error) {
		collected++
		return nil, errors.New("not answered")
	}
	fi := schedule.Firing{Tenant: "t1", Schedule: "morning", Agent: "payouts", Input: "Reconcile payouts.",
		Due: time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)}
	r, err := f.runner.Start(ctx, server.Start{Tenant: "t1", Agent: "payouts", Trigger: run.BySchedule,
		Source: "morning", Input: fi.Input, ID: fi.Key()})
	if err != nil {
		t.Fatal(err)
	}
	paused := r
	paused.Status, paused.Version = run.Paused, r.Version+1
	paused.Requirements = []run.Requirement{{CallID: "c1", Answered: fi.Due}} // answered: claimable
	if err := runs.Put(ctx, r, paused); err != nil {
		t.Fatal(err)
	}
	if err := f.fire(ctx, fi); err != nil {
		t.Fatal(err)
	}
	got, err := runs.Get(ctx, "t1", fi.Key())
	if err != nil || got.Status != run.Paused || got.Version != paused.Version || collected != 0 {
		t.Fatalf("got %+v (%v), collected %d: a repeated firing must leave a paused run alone", got, err, collected)
	}
}

// windowedReplies is a model that knows its context window.
type windowedReplies struct{ replies }

func (*windowedReplies) Window(_ context.Context, id string) (int64, error) {
	if id != "claude-opus-5-5" {
		return 0, errors.New("unknown model")
	}
	return 1_000_000, nil
}

// The model on Lambda gives the run's context report its window, made with
// its key like a turn (#77): the agent asks it through model.Windower.
func TestTheKeyedModelKnowsTheWindow(t *testing.T) {
	var _ model.Windower = (*keyedModel)(nil)
	reads := 0
	k := &keyedModel{
		key:  func(context.Context) (string, error) { reads++; return "sk-model", nil },
		make: func(string) model.Model { return &windowedReplies{} },
	}
	if w, err := k.Window(ctx, "claude-opus-5-5"); err != nil || w != 1_000_000 {
		t.Fatalf("window %d, %v", w, err)
	}
	if _, err := k.Turn(ctx, model.Request{}, nil); err == nil {
		t.Fatal("the scripted model had a reply") // replies is empty: the turn reaches the same model
	}
	if reads != 1 {
		t.Fatalf("%d key reads", reads)
	}
	plain := &keyedModel{key: func(context.Context) (string, error) { return "k", nil },
		make: func(string) model.Model { return &replies{} }}
	if _, err := plain.Window(ctx, "claude-opus-5-5"); err == nil {
		t.Fatal("a model with no window gave one")
	}
}

// A firing of a halted agent is skipped, not retried: a retry would run it
// once resumed, and nothing from before a halt runs.
func TestAFiringOfAHaltedAgentIsSkipped(t *testing.T) {
	f, _, agents, runs := newFunctionWith(t, &replies{}, &invocations{})
	_ = agents.Put(ctx, registrytest.Definition("t1", "payouts"))
	if _, err := agents.Halt(ctx, "t1", "payouts", time.Date(2026, 10, 19, 6, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	fi := schedule.Firing{Tenant: "t1", Schedule: "morning", Agent: "payouts", Input: "Reconcile payouts.",
		Due: time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)}
	// Skipped, and said so: the schedule keeps it as skipped_halted, never as a run.
	if err := f.fire(ctx, fi); !errors.Is(err, schedule.ErrHalted) {
		t.Fatalf("a firing of a halted agent = %v; want schedule.ErrHalted", err)
	}
	if _, err := runs.Get(ctx, "t1", fi.Key()); !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("a halted agent's firing stored a run: %v", err)
	}
}

// A firing while the account's tafs wait for credits is skipped, not
// retried: KIFF answered, and what the firing was handed waits for the next
// one that runs (skipped_credits). KIFF not answering is an error, so the
// firing is tried again.
func TestAFiringWhileTheTafsWaitIsSkipped(t *testing.T) {
	f, _, agents, runs := newFunctionWith(t, &replies{}, &invocations{})
	_ = agents.Put(ctx, registrytest.Definition("t1", "payouts"))
	fi := schedule.Firing{Tenant: "t1", Schedule: "morning", Agent: "payouts", Input: "Reconcile payouts.",
		Due: time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)}
	rn := f.runner.(*runner.Runner)
	rn.Allowance = func(context.Context, string, string) (bool, string, error) { return false, "credits_used_up", nil }
	var credits *schedule.CreditsError
	if err := f.fire(ctx, fi); !errors.As(err, &credits) || credits.Why != "credits_used_up" {
		t.Fatalf("a firing while the tafs wait = %v; want a schedule.CreditsError", err)
	}
	if _, err := runs.Get(ctx, "t1", fi.Key()); !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("a refused firing stored a run: %v", err)
	}
	rn.Allowance = func(context.Context, string, string) (bool, string, error) { return false, "", errors.New("down") }
	if err := f.fire(ctx, fi); err == nil || errors.As(err, &credits) {
		t.Fatalf("KIFF down = %v; want an error the firing is tried again on", err)
	}
}

// A firing of an agent switching versions is skipped, not retried, and says
// so (schedule.ErrSwitching): never a run that wasn't made. So is a repeated
// delivery of one whose run a switch stopped before it began, and that one
// never counts as a run that failed.
func TestAFiringOfASwitchingAgentIsSkippedSwitching(t *testing.T) {
	f, _, agents, runs := newFunctionWith(t, &replies{}, &invocations{})
	_ = agents.Put(ctx, registrytest.Definition("t1", "payouts"))
	if _, err := agents.BeginSwitch(ctx, "t1", "payouts", registry.Switching{N: 2, At: time.Date(2026, 10, 19, 6, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	fi := schedule.Firing{Tenant: "t1", Schedule: "morning", Agent: "payouts", Input: "Reconcile payouts.",
		Due: time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)}
	if err := f.fire(ctx, fi); !errors.Is(err, schedule.ErrSwitching) {
		t.Fatalf("a firing of a switching agent = %v; want schedule.ErrSwitching", err)
	}
	if _, err := runs.Get(ctx, "t1", fi.Key()); !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("a switching agent's firing stored a run: %v", err)
	}
	// A run stored, then stopped by a switch before it began: delivered again, it's the switch's skip.
	stopped := run.Run{ID: fi.Key(), Tenant: "t1", Agent: "payouts", Trigger: run.BySchedule, Source: "morning",
		Status: run.Error, Error: runner.FailedSwitching, Version: 1}
	if err := runs.Create(ctx, stopped); err != nil {
		t.Fatal(err)
	}
	if err := f.fire(ctx, fi); !errors.Is(err, schedule.ErrSwitching) {
		t.Fatalf("a repeated delivery of a switch-stopped run = %v; want schedule.ErrSwitching", err)
	}
}

// switchOnCreate begins the agent's switch as soon as a run is stored: the
// run starts, then is stopped before its loop by the switch.
type switchOnCreate struct {
	*memory.Runs
	agents *memory.Agents
}

func (s switchOnCreate) Create(ctx context.Context, r run.Run) error {
	if err := s.Runs.Create(ctx, r); err != nil {
		return err
	}
	_, err := s.agents.BeginSwitch(ctx, r.Tenant, r.Agent, registry.Switching{N: 2, At: time.Date(2026, 10, 19, 6, 0, 0, 0, time.UTC)})
	return err
}

// A firing whose run started, then was stopped before its loop by a switch
// begun meanwhile, is the switch's skip, never a run that did nothing.
func TestAFiringStoppedByASwitchBegunMeanwhileIsSkippedSwitching(t *testing.T) {
	f, _, agents, runs := newFunctionWith(t, &replies{}, &invocations{})
	_ = agents.Put(ctx, registrytest.Definition("t1", "payouts"))
	f.runner.(*runner.Runner).Runs = switchOnCreate{Runs: runs, agents: agents}
	fi := schedule.Firing{Tenant: "t1", Schedule: "morning", Agent: "payouts", Input: "Reconcile payouts.",
		Due: time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)}
	if err := f.fire(ctx, fi); !errors.Is(err, schedule.ErrSwitching) {
		t.Fatalf("a firing stopped by a switch = %v; want schedule.ErrSwitching", err)
	}
	if r, err := runs.Get(ctx, "t1", fi.Key()); err != nil || r.Error != runner.FailedSwitching {
		t.Fatalf("its run = %+v, %v; want it stopped for the switch", r, err)
	}
}

// Each firing's run is told, from the runtime and not its notes, when its
// schedule's last run that did any work ended and how, with the start of its
// last answer; a run with no work behind it is told so. Runs still at work,
// paused for the owner, or that did nothing are passed over, and so is the
// firing's own run.
func TestAFiringIsToldHowTheLastRunThatWorkedEnded(t *testing.T) {
	m := &replies{next: []model.Reply{answer("Done."), answer("Done.")}}
	f, _, agents, runs := newFunctionWith(t, m, &invocations{})
	_ = agents.Put(ctx, registrytest.Definition("t1", "payouts"))
	at := func(h int) time.Time { return time.Date(2026, 11, 9, h, 0, 0, 0, time.UTC) }
	fire := func(sched string, due time.Time) string {
		t.Helper()
		fi := schedule.Firing{Tenant: "t1", Schedule: sched, Agent: "payouts", Input: "Work the refund queue.", Due: due}
		if err := f.fire(ctx, fi); err != nil {
			t.Fatal(err)
		}
		r, err := runs.Get(ctx, "t1", fi.Key())
		if err != nil {
			t.Fatal(err)
		}
		return r.Input
	}
	if got := fire("fresh", at(8)); got != "Work the refund queue.\n\n[From the runtime, not your notes: this schedule has no earlier run that did any work.]" {
		t.Fatalf("first firing's input %q", got)
	}
	// Runs that did nothing, or that haven't ended, aren't the last that worked.
	put := func(r run.Run) {
		t.Helper()
		r.Tenant, r.Agent, r.Trigger, r.Source, r.Version = "t1", "payouts", run.BySchedule, "queue", 1
		if err := runs.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	put(run.Run{ID: "cut", Status: run.Completed, Stop: "turn_cap", Turns: 50, Output: "Refunded orders 1 to 20.\n\nStill open: 21 to 40.",
		Created: at(9), Updated: at(9).Add(10 * time.Minute), Ended: at(9).Add(10 * time.Minute)})
	put(run.Run{ID: "nothing", Status: run.Error, Error: runner.FailedRun, Turns: 0, Created: at(10), Updated: at(10)})
	put(run.Run{ID: "paused", Status: run.Paused, Turns: 3, Created: at(11), Updated: at(11)})
	got := fire("queue", at(12))
	want := "Work the refund queue.\n\n[From the runtime, not your notes: your last run on this schedule that did any work ended 2026-11-09 09:10 UTC; " +
		"it stopped at its turn limit, cut short before it finished. Its last answer began, in its own words, not the runtime's: \"Refunded orders 1 to 20. Still open: 21 to 40.\"]"
	if got != want {
		t.Fatalf("input\n%q\nwant\n%q", got, want)
	}
}

// How a run ended, in the words the next firing reads.
func TestHandOverSaysHowARunEnded(t *testing.T) {
	end := time.Date(2026, 11, 9, 9, 10, 0, 0, time.UTC)
	for _, c := range []struct {
		r    run.Run
		want string
	}{
		{run.Run{Status: run.Completed, Stop: "answered"}, "it answered and finished"},
		{run.Run{Status: run.Completed}, "it answered and finished"},
		{run.Run{Status: run.Completed, Stop: "turn_cap"}, "it stopped at its turn limit, cut short before it finished"},
		{run.Run{Status: run.Completed, Stop: "cost_cap"}, "it stopped at its cost limit, cut short before it finished"},
		{run.Run{Status: run.Completed, Stop: "max_token"}, "it ran out of room mid-answer, cut short"},
		{run.Run{Status: run.Completed, Stop: "refused"}, "a turn was declined, so it stopped before it finished"},
		{run.Run{Status: run.Completed, Stop: "something_new"}, "it stopped (something_new) before it finished"},
		{run.Run{Status: run.Error, Error: runner.FailedRun}, "it failed after doing some work"},
		{run.Run{Status: run.Cancelled}, "it was stopped by its owner before it finished"},
	} {
		c.r.Ended = end
		got := handOver(c.r)
		if !strings.Contains(got, "ended 2026-11-09 09:10 UTC; "+c.want+".") || strings.Contains(got, "Its last answer") {
			t.Errorf("%+v: %q", c.r, got)
		}
	}
	// A long answer is cut, on a rune, and marked; one stored before Ended was kept ends when it last changed.
	long := handOver(run.Run{Status: run.Completed, Stop: "answered", Updated: end, Output: strings.Repeat("é", 300)})
	if !strings.Contains(long, "ended 2026-11-09 09:10 UTC") || !strings.HasSuffix(long, "…\"]") || !utf8.ValidString(long) {
		t.Fatalf("long %q", long)
	}
}

// A check's read that fails while the agent is halted or switching is that
// skip, never a read that failed; any other failure is the read's own error,
// and a read that worked is passed on whatever the agent's state.
func TestACheckReadRefusedForAHaltOrASwitchIsThatSkip(t *testing.T) {
	refused := errors.New("lambda: the check's read gmail__list wasn't made: halted")
	for _, c := range []struct {
		name      string
		read      error
		startable error
		want      error
	}{
		{"halted", refused, fmt.Errorf("%w", registry.ErrHalted), schedule.ErrHalted},
		{"switching", refused, fmt.Errorf("%w (to version 2)", registry.ErrSwitching), schedule.ErrSwitching},
		{"neither", refused, nil, refused},
		{"the state can't be read", refused, errors.New("dynamodb down"), refused},
	} {
		read := checkRead(func(context.Context, string, string) error { return c.startable },
			func(context.Context, string, string, schedule.Check) (string, error) { return "", c.read })
		if _, err := read(ctx, "t1", "inbox", schedule.Check{}); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	// A halted or switching agent's check reads nothing: the app isn't called.
	for _, c := range []struct {
		name      string
		startable error
		want      error
	}{
		{"halted", fmt.Errorf("%w", registry.ErrHalted), schedule.ErrHalted},
		{"switching", fmt.Errorf("%w (to version 2)", registry.ErrSwitching), schedule.ErrSwitching},
	} {
		reads := 0
		read := checkRead(func(context.Context, string, string) error { return c.startable },
			func(context.Context, string, string, schedule.Check) (string, error) {
				reads++
				return `{"data":[]}`, nil
			})
		if _, err := read(ctx, "t1", "inbox", schedule.Check{}); !errors.Is(err, c.want) || reads != 0 {
			t.Errorf("%s before the read: %v, want %v (%d reads)", c.name, err, c.want, reads)
		}
	}
	// One that's neither reads, and a read that works is the answer; a state
	// that can't be read doesn't stop the read.
	for name, st := range map[string]error{"startable": nil, "the state can't be read": errors.New("dynamodb down")} {
		worked := checkRead(func(context.Context, string, string) error { return st },
			func(context.Context, string, string, schedule.Check) (string, error) { return `{"data":[]}`, nil })
		if got, err := worked(ctx, "t1", "inbox", schedule.Check{}); err != nil || got != `{"data":[]}` {
			t.Fatalf("%s: a read that worked: %q, %v", name, got, err)
		}
	}
	// A halt that begins during a read that fails is still the halt's skip.
	asked := 0
	during := checkRead(func(context.Context, string, string) error {
		if asked++; asked == 1 {
			return nil
		}
		return registry.ErrHalted
	}, func(context.Context, string, string, schedule.Check) (string, error) { return "", refused })
	if _, err := during(ctx, "t1", "inbox", schedule.Check{}); !errors.Is(err, schedule.ErrHalted) || asked != 2 {
		t.Fatalf("a halt during a failed read: %v (asked %d times)", err, asked)
	}
}

type handedRuns map[string]run.Run

func (h handedRuns) Get(_ context.Context, _, id string) (run.Run, error) {
	r, ok := h[id]
	if !ok {
		return run.Run{}, run.ErrNotFound
	}
	return r, nil
}

// handedEnded is how the next firing learns how a run started by hand with
// a check's items ended: not done while it goes, let go once it's gone, and
// a final one answers what a firing's own run would (ended).
func TestHandedEndedSaysHowARunStartedByHandEnded(t *testing.T) {
	runs := handedRuns{
		"going":    {ID: "going", Status: run.Running},
		"worked":   {ID: "worked", Status: run.Completed, Turns: 3},
		"unworked": {ID: "unworked", Status: run.Error, Error: runner.FailedSwitching},
	}
	for id, want := range map[string]struct {
		done bool
		err  error
	}{
		"going":    {false, nil},
		"gone":     {true, nil},
		"worked":   {true, nil},
		"unworked": {true, schedule.ErrSwitching},
	} {
		done, err := handedEnded(context.Background(), runs, "t1", id)
		if done != want.done || !errors.Is(err, want.err) || (want.err == nil && err != nil) {
			t.Errorf("%s: done %v, err %v; want %v, %v", id, done, err, want.done, want.err)
		}
	}
}
