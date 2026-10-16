// Command lambda is the server on AWS Lambda: one function behind a function
// URL with AWS_IAM auth and invoke mode RESPONSE_STREAM, so server-sent
// events reach the client as they happen, which also takes EventBridge
// Scheduler's firings and executes background runs, each sent to it by an
// asynchronous invoke of itself.
//
// Everything account-specific comes from the environment, never from code:
//
//	AGENTS_TABLE      the DynamoDB table (store/dynamodb's)
//	AGENTS_CALLERS    assumed-role ARN prefixes allowed to call, comma-separated (lambdaurl.Auth)
//	AGENTS_GATEWAYS   the gateways an agent may name, comma-separated; KIFF's by default
//	SCHEDULE_GROUP    EventBridge Scheduler's schedule group
//	SCHEDULE_TARGET   the function each firing invokes: this one
//	SCHEDULE_ROLE     the role EventBridge Scheduler invokes it with
//	AGENTS_SECRETS_PREFIX    the name prefix of the agents' KIFF keys in Secrets Manager (registry/secretsmanager)
//	AGENTS_MODEL_KEY_SECRET  the name of the secret holding the model provider's key; not under AGENTS_SECRETS_PREFIX
//
// An EventBridge Scheduler schedule sends {"sweep":{}} every minute, through
// SCHEDULE_ROLE: the sweep ends the runs nobody else is left to end, and
// sends paused runs whose held calls KIFF resolved to the background to
// continue (cmd/kiffalerts records the answers).
//
// Lambda sets AWS_LAMBDA_FUNCTION_NAME itself: a background run is an
// asynchronous invoke of this same function, with a run event (runs.go).
//
// Who may invoke the function is the only guard on who the caller is: the
// function reads the signer Lambda verified from the event, and a direct
// invoke can send any event, a made-up function URL request naming tafs
// cloud's role and any tenant included. The code can't tell the two apart, so
// the function's resource policy and IAM must hold this:
//
//   - Only SCHEDULE_ROLE, and the function's own role on this function (to
//     send background runs), may call lambda:InvokeFunction directly. A run
//     event only executes a stored run once, as a request would.
//   - The function's asynchronous retries are off (MaximumRetryAttempts 0).
//   - Its role reads only secrets under AGENTS_SECRETS_PREFIX and the model
//     key's secret.
//   - tafs cloud's role gets lambda:InvokeFunctionUrl, plus
//     lambda:InvokeFunction only under the lambda:InvokedViaFunctionUrl
//     condition, if the account's function URL authorization needs both.
//   - iam:PassRole on SCHEDULE_ROLE is limited to the operators who deploy,
//     so nobody else can make a schedule that invokes the function with an
//     event of their own.
//
// AGENTS_CALLERS ARNs are in the aws partition (arn:aws:sts::), as in every
// region tafs uses.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	sch "github.com/aws/aws-sdk-go-v2/service/scheduler"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/model/anthropic"
	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/registry/secretsmanager"
	"github.com/kiff/agents/runner"
	"github.com/kiff/agents/schedule"
	"github.com/kiff/agents/schedule/eventbridge"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/server/lambdaurl"
	"github.com/kiff/agents/store/dynamodb"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/kiff"
	toolmemory "github.com/kiff/agents/tool/memory"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	fn, err := setup(context.Background(), log)
	if err != nil {
		log.Error("setup", "err", err)
		os.Exit(1)
	}
	lambda.Start(fn.handle)
}

// function is what each invocation runs.
type function struct {
	http http.Handler
	// api is the server http serves; setup keeps it so its wiring can be read.
	api       *server.Server
	schedules *schedule.Service
	runner    server.Runner
	sweeper   func(ctx context.Context, now time.Time) (int, error)
	// cont is how the sweep continues a resolved paused run; setup keeps it
	// so its wiring can be read.
	cont func(ctx context.Context, tenant, id string) error
	// now is the clock; nil is time.Now.
	now func() time.Time
	log *slog.Logger
}

func setup(ctx context.Context, log *slog.Logger) (*function, error) {
	env := func(name string) (string, error) {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			return "", fmt.Errorf("set %s", name)
		}
		return v, nil
	}
	var missing []error
	need := func(name string) string {
		v, err := env(name)
		missing = append(missing, err)
		return v
	}
	table, callers := need("AGENTS_TABLE"), need("AGENTS_CALLERS")
	group, target, role := need("SCHEDULE_GROUP"), need("SCHEDULE_TARGET"), need("SCHEDULE_ROLE")
	prefix, keySecret := need("AGENTS_SECRETS_PREFIX"), need("AGENTS_MODEL_KEY_SECRET")
	self := need("AWS_LAMBDA_FUNCTION_NAME")
	if err := errors.Join(missing...); err != nil {
		return nil, err
	}
	if err := outsidePrefix(keySecret, prefix); err != nil {
		return nil, err
	}
	gateways := []string{kiff.URL}
	if v := strings.TrimSpace(os.Getenv("AGENTS_GATEWAYS")); v != "" {
		gateways = split(v)
	}
	auth, err := lambdaurl.Auth(split(callers))
	if err != nil {
		return nil, err
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	db := ddb.NewFromConfig(cfg)
	store := dynamodb.New(db, table)
	agents := dynamodb.NewAgents(db, table)
	runs := dynamodb.NewRuns(db, table)
	secrets := sm.NewFromConfig(cfg)
	llm := &keyedModel{
		key:  func(ctx context.Context) (string, error) { return modelKey(ctx, secrets, keySecret) },
		make: func(key string) model.Model { return anthropic.New(option.WithAPIKey(key)) },
	}
	keys := secretsmanager.New(secrets, prefix)
	rn := &runner.Runner{Runs: runs, Sessions: store, Builder: &registry.Builder{
		Agents: agents, Secrets: keys,
		Model: llm, Gateways: gateways,
		Connect: func(ctx context.Context, gateway, key string) ([]tool.Tool, error) {
			return kiff.New(gateway, key, nil).Tools(ctx)
		},
		Own: func(tenant, agent string) []tool.Tool { return []tool.Tool{toolmemory.New(store, tenant, agent)} },
	}, Collect: kiff.Collect}
	schedules := &schedule.Service{
		Store:     dynamodb.NewSchedules(db, table),
		Scheduler: eventbridge.New(sch.NewFromConfig(cfg), group, target, role),
		// The registry's Get is a consistent read, as Fire needs: it deletes
		// the schedules of an agent Live reports gone.
		Live: func(ctx context.Context, tenant, agent string) (bool, error) {
			_, err := agents.Get(ctx, tenant, agent)
			if errors.Is(err, registry.ErrNotFound) {
				return false, nil
			}
			return err == nil, err
		},
	}
	bg := background(lambdasvc.NewFromConfig(cfg), self)
	f := &function{
		api: &server.Server{Auth: auth, Agents: agents, Sessions: store, Notes: store, Schedules: schedules,
			Runs: runs, Runner: rn, Background: bg, Gateways: gateways, Log: log,
			// Cancelling a paused run, or removing an agent with paused runs,
			// takes back its held calls in KIFF with the agent's own key.
			Withdraw: kiff.Withdrawer(agents, keys, &http.Client{Timeout: withdrawTimeout}, kiff.APIURL)},
		schedules: schedules,
		runner:    rn,
		cont:      bg,
		log:       log,
	}
	f.http = f.api.Handler()
	f.sweeper = sweeping(runs, rn, f.cont)
	// A firing starts its run and executes it in its own invocation.
	schedules.Start = f.fire
	return f, nil
}

// withdrawTimeout bounds one withdrawal from KIFF: a cancel waits on each of
// its run's held calls in turn.
const withdrawTimeout = 15 * time.Second

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// handle runs one invocation: a request through the function URL, a
// background run, the sweep, or a schedule's firing. Anything else is
// refused.
func (f *function) handle(ctx context.Context, raw json.RawMessage) (any, error) {
	var shape struct {
		RequestContext *struct {
			HTTP *struct{} `json:"http"`
		} `json:"requestContext"`
		Run      json.RawMessage `json:"run"`
		Sweep    json.RawMessage `json:"sweep"`
		Schedule string          `json:"schedule"`
		Due      string          `json:"due"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil {
		return nil, fmt.Errorf("lambda: the event is not JSON: %w", err)
	}
	switch {
	case shape.RequestContext != nil && shape.RequestContext.HTTP != nil:
		var e events.LambdaFunctionURLRequest
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, err
		}
		return lambdaurl.Serve(ctx, f.http, e, f.log)
	case len(shape.Run) > 0:
		var e runEvent
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("lambda: a run event that isn't one: %w", err)
		}
		return nil, f.execute(ctx, e.Run.Tenant, e.Run.ID)
	case len(shape.Sweep) > 0:
		return nil, f.sweep(ctx)
	case shape.Schedule != "" && shape.Due != "":
		tenant, id, due, err := eventbridge.ParseEvent(raw)
		if err != nil {
			return nil, err
		}
		if err := f.schedules.Fire(ctx, tenant, id, due); err != nil {
			f.log.ErrorContext(ctx, "firing", "schedule", eventbridge.Name(tenant, id), "err", err)
			return nil, err
		}
		return nil, nil
	}
	return nil, errors.New("lambda: an event this function doesn't take")
}
