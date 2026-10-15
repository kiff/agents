// Command lambda is the server on AWS Lambda: one function behind a function
// URL with AWS_IAM auth, which also takes EventBridge Scheduler's firings.
//
// Everything account-specific comes from the environment, never from code:
//
//	AGENTS_TABLE      the DynamoDB table (store/dynamodb's)
//	AGENTS_CALLERS    assumed-role ARN prefixes allowed to call, comma-separated (lambdaurl.Auth)
//	AGENTS_GATEWAYS   the gateways an agent may name, comma-separated; KIFF's by default
//	SCHEDULE_GROUP    EventBridge Scheduler's schedule group
//	SCHEDULE_TARGET   the function each firing invokes: this one
//	SCHEDULE_ROLE     the role EventBridge Scheduler invokes it with
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

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	sch "github.com/aws/aws-sdk-go-v2/service/scheduler"

	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/schedule"
	"github.com/kiff/agents/schedule/eventbridge"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/server/lambdaurl"
	"github.com/kiff/agents/store/dynamodb"
	"github.com/kiff/agents/tool/kiff"
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
	http      http.Handler
	schedules *schedule.Service
	log       *slog.Logger
}

// errNoRuns is what a firing gets until runs can start: the firing is
// checked against the stored schedule (and the Scheduler healed) first.
var errNoRuns = errors.New("lambda: a scheduled run can't start yet: runs arrive with the run routes (#13)")

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
	if err := errors.Join(missing...); err != nil {
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
		Start: func(context.Context, schedule.Firing) error { return errNoRuns },
	}
	return &function{
		http: (&server.Server{Auth: auth, Agents: agents, Sessions: store, Notes: store, Schedules: schedules,
			Gateways: gateways, Log: log}).Handler(),
		schedules: schedules,
		log:       log,
	}, nil
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// handle runs one invocation: a request through the function URL, or a
// schedule's firing. Anything else is refused.
func (f *function) handle(ctx context.Context, raw json.RawMessage) (any, error) {
	var shape struct {
		RequestContext *struct {
			HTTP *struct{} `json:"http"`
		} `json:"requestContext"`
		Schedule string `json:"schedule"`
		Due      string `json:"due"`
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
		return lambdaurl.Serve(ctx, f.http, e)
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
