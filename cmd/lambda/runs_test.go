package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/registry/registrytest"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/runner"
	"github.com/kiff/agents/server"
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
	f, _, agents, runs := newFunctionWith(t, &replies{}, &invocations{})
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
	healthy := start()
	if _, err := run.Claim(ctx, runs, "t1", healthy.ID, "c2", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}

	// Past PendingTimeout and the crashed run's lease, inside the healthy one's.
	later := now.Add(run.PendingTimeout + run.LeaseGrace + 2*time.Minute)
	f.now = func() time.Time { return later }
	if _, err := f.handle(ctx, json.RawMessage(`{"sweep":{}}`)); err != nil {
		t.Fatal(err)
	}

	want := later.Add(d.Keep.SessionRetention())
	for _, r := range []run.Run{unclaimed, crashed} {
		got, err := runs.Get(ctx, "t1", r.ID)
		if err != nil || got.Status != run.Error || !got.Expires.Equal(want) {
			t.Fatalf("got %s %s expiring %v (%v), want ERROR expiring %v", got.ID, got.Status, got.Expires, err, want)
		}
	}
	if st, err := rn.Sessions.State(ctx, "t1", crashed.Session, crashed.ID); err != nil || !st.Ended {
		t.Fatalf("the crashed run's recording: %+v (%v)", st, err)
	}
	if got, _ := runs.Get(ctx, "t1", healthy.ID); got.Status != run.Running {
		t.Fatalf("a run still in its lease was swept: %s", got.Status)
	}
}

// failingRuns is a run store whose Due fails.
type failingRuns struct{ run.Runs }

func (failingRuns) Due(context.Context, time.Time, int) ([]run.Run, error) {
	return nil, errors.New("throttled")
}

func TestAFailedSweepShows(t *testing.T) {
	f, _, _, runs := newFunctionWith(t, &replies{}, &invocations{})
	f.sweeper = sweeping(failingRuns{runs}, f.runner.(*runner.Runner))
	if _, err := f.handle(ctx, json.RawMessage(`{"sweep":{}}`)); err == nil {
		t.Fatal("a sweep that read nothing succeeded")
	}
}
