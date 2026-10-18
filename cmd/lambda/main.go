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
//	AGENTS_TENANT_RUNS  how many runs a tenant may have going at once (PENDING or RUNNING);
//	                  10 by default. A request over it is 429.
//	AGENTS_KIFF_API   the KIFF API held calls are withdrawn from; KIFF's by default, and
//	                  required when AGENTS_GATEWAYS is set, so the two name the same KIFF
//	SCHEDULE_GROUP    EventBridge Scheduler's schedule group
//	SCHEDULE_TARGET   the function each firing invokes: this one
//	SCHEDULE_ROLE     the role EventBridge Scheduler invokes it with
//	AGENTS_SECRETS_PREFIX    the name prefix of the agents' KIFF keys in Secrets Manager (registry/secretsmanager)
//	AGENTS_MODEL_KEY_SECRET  the name of the secret holding the model provider's key; not under AGENTS_SECRETS_PREFIX
//	AGENTS_DRAFTS_PREFIX     the name prefix of taf drafts' claim secrets (#91); not under AGENTS_SECRETS_PREFIX.
//	                         Unset: no agent gets the draft tool, and POST /drafts/{id}/claim isn't served
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
	"net/url"
	"os"
	"strconv"
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

	"github.com/kiff/agents/drafts"
	draftsapi "github.com/kiff/agents/drafts/kiffapi"
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
	tooldrafts "github.com/kiff/agents/tool/drafts"
	"github.com/kiff/agents/tool/kiff"
	toolmemory "github.com/kiff/agents/tool/memory"
	"github.com/kiff/agents/tool/planque"
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
	// kiffAPI is the KIFF API Withdraw goes to; setup keeps it so its wiring
	// can be read.
	kiffAPI string
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
	// Planques an agent may ask for vendor access through (#91); none
	// unless set, so no agent gets the planque tool by default.
	planques := split(os.Getenv("AGENTS_PLANQUES"))
	maxActive, err := tenantRuns(os.Getenv("AGENTS_TENANT_RUNS"))
	if err != nil {
		return nil, err
	}
	kiffAPI, err := kiffAPIFor(os.Getenv("AGENTS_GATEWAYS"), os.Getenv("AGENTS_KIFF_API"))
	if err != nil {
		return nil, err
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
	var claims *drafts.Service
	if dp := strings.TrimSpace(os.Getenv("AGENTS_DRAFTS_PREFIX")); dp != "" {
		if strings.HasPrefix(dp, prefix) || strings.HasPrefix(prefix, dp) {
			return nil, fmt.Errorf("lambda: AGENTS_DRAFTS_PREFIX %q and AGENTS_SECRETS_PREFIX %q must not overlap: a key reference could name a claim secret", dp, prefix)
		}
		claims = &drafts.Service{Store: secretsmanager.NewDrafts(secrets, dp), Keys: secretsmanager.NewKeys(keys, secrets),
			KIFF: draftsapi.New(kiffAPI, nil), Log: log}
	}
	rn := &runner.Runner{Runs: runs, Sessions: store, Builder: &registry.Builder{
		Agents: agents, Secrets: keys,
		Model: llm, Gateways: gateways,
		Connect: func(ctx context.Context, gateway, key string) ([]tool.Tool, error) {
			return kiff.New(gateway, key, nil).Tools(ctx)
		},
		Own:      func(tenant, agent string) []tool.Tool { return []tool.Tool{toolmemory.New(store, tenant, agent)} },
		Drafts:   draftTool(claims),
		Planques: planques,
		Planque: func(base, tenant, key string) tool.Tool {
			return planque.New(base, tenant, key, nil)
		},
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
			Runs: runs, Runner: rn, Background: bg, Gateways: gateways, MaxActive: maxActive, Log: log, Drafts: claimsOrNil(claims)},
		kiffAPI:   kiffAPI,
		schedules: schedules,
		runner:    rn,
		cont:      bg,
		log:       log,
	}
	// Cancelling a paused run, or removing an agent with paused runs, takes
	// back its held calls in KIFF with the agent's own key.
	f.api.Withdraw = kiff.Withdrawer(agents, keys, &http.Client{Timeout: withdrawTimeout}, f.kiffAPI)
	f.http = f.api.Handler()
	f.sweeper = sweeping(runs, rn, f.cont)
	// A firing starts its run and executes it in its own invocation.
	schedules.Start = f.fire
	return f, nil
}

// draftTool is the draft tool on claims, or nil when drafts are off.
func draftTool(claims *drafts.Service) func(tenant, key string) tool.Tool {
	if claims == nil {
		return nil
	}
	return func(tenant, key string) tool.Tool { return tooldrafts.New(claims, tenant, key) }
}

// claimsOrNil keeps a nil service a nil interface, so the claim route is
// not served.
func claimsOrNil(claims *drafts.Service) server.Drafts {
	if claims == nil {
		return nil
	}
	return claims
}

// defaultTenantRuns is how many runs a tenant may have going at once when
// AGENTS_TENANT_RUNS isn't set.
const defaultTenantRuns = 10

// tenantRuns reads AGENTS_TENANT_RUNS: a whole number, 1 or more.
func tenantRuns(v string) (int, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return defaultTenantRuns, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("AGENTS_TENANT_RUNS %q: a whole number, 1 or more", v)
	}
	return n, nil
}

// kiffAPIFor is the KIFF API held calls are withdrawn from: api, or KIFF's
// own when neither it nor gateways is set. A withdrawal sends the agent's
// KIFF key there, and must reach the KIFF that holds the call: with gateways
// set, an agent's calls may be held by another KIFF (a staging one, say), so
// api must be set too. It must be https, with a host and nothing after it.
func kiffAPIFor(gateways, api string) (string, error) {
	api = strings.TrimSpace(api)
	if api == "" {
		if strings.TrimSpace(gateways) != "" {
			return "", errors.New("set AGENTS_KIFF_API: AGENTS_GATEWAYS is set, so held calls may not be KIFF's own to withdraw")
		}
		return kiff.APIURL, nil
	}
	// kiff.Withdraw appends its route to this as a string, so anything after
	// the host would move it: a '?' or '#', even with nothing after it, makes
	// the route a query or a fragment, and extra slashes another path. Its
	// 404 would read as no hold to withdraw, and the call would stay live.
	// So only "https://host" is taken, or with one trailing slash, and what
	// is returned is built from the parsed host, not the spelling given.
	u, err := url.Parse(api)
	bad := err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || strings.ContainsAny(api, "?#")
	if bad {
		return "", fmt.Errorf("AGENTS_KIFF_API %q: https:// and a host, nothing after it", api)
	}
	return "https://" + u.Host, nil
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
