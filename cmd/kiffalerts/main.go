// Command kiffalerts receives KIFF's alerts on AWS Lambda: POST /kiff/alerts,
// and nothing else, behind a function URL with no IAM auth (AuthType NONE),
// since KIFF signs each alert with the alert target's own secret, not with
// AWS. kiffalert.Handler checks that signature and records the owner's answer
// on the run that paused on the hold. That is all it does: a run that waits
// for nothing more is due a minute later, and the agents function's sweep
// (cmd/lambda) continues it. This function can execute nothing.
//
// The function is apart from the agents function so that its open URL can
// serve no other route: this command mounts only kiffalert.Handler.
//
// Everything account-specific comes from the environment, never from code:
//
//	AGENTS_TABLE           the DynamoDB table (store/dynamodb's), for the runs
//	KIFF_ACCOUNTS_PREFIX   the name prefix of the KIFF accounts' secrets (registry/secretsmanager.Accounts)
//	AGENTS_SECRETS_PREFIX  the agents' key prefix, which KIFF_ACCOUNTS_PREFIX must stay apart from
//
// Its role must hold no more than this:
//
//   - Read and write on AGENTS_TABLE.
//   - secretsmanager:GetSecretValue on KIFF_ACCOUNTS_PREFIX only.
//   - No lambda:InvokeFunction on the agents function. A direct invoke there
//     can send any event, a made-up function URL request for any tenant
//     included, and this function's URL is open to anyone.
//
// The function's URL is AuthType NONE, and its invoke mode BUFFERED. Its
// reserved concurrency should be low: anyone can post to it, and each post
// costs an invocation and a secret read, from the same Secrets Manager quota
// the agents function reads its keys from.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/kiff/agents/registry/secretsmanager"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/server/kiffalert"
	"github.com/kiff/agents/server/lambdaurl"
	"github.com/kiff/agents/store/dynamodb"
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
}

func setup(ctx context.Context, log *slog.Logger) (*function, error) {
	var missing []error
	need := func(name string) string {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			missing = append(missing, fmt.Errorf("set %s", name))
		}
		return v
	}
	table, accounts := need("AGENTS_TABLE"), need("KIFF_ACCOUNTS_PREFIX")
	keys := need("AGENTS_SECRETS_PREFIX")
	if err := errors.Join(missing...); err != nil {
		return nil, err
	}
	if err := apart(accounts, keys); err != nil {
		return nil, err
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	return newFunction(secretsmanager.NewAccounts(sm.NewFromConfig(cfg), accounts),
		dynamodb.NewRuns(ddb.NewFromConfig(cfg), table), log), nil
}

// apart refuses an accounts prefix that overlaps the agents' key prefix: an
// agent's key reference could then name a signing secret, or an alert's
// account an agent's key.
func apart(accounts, keys string) error {
	if strings.HasPrefix(accounts, keys) || strings.HasPrefix(keys, accounts) {
		return fmt.Errorf("kiffalerts: KIFF_ACCOUNTS_PREFIX %q and AGENTS_SECRETS_PREFIX %q must not overlap", accounts, keys)
	}
	return nil
}

// newFunction serves the alerts with no Continue: the answer is recorded, and
// the sweep continues the run.
func newFunction(accounts kiffalert.Accounts, runs run.Runs, log *slog.Logger) *function {
	return &function{http: only{path: "/kiff/alerts",
		h: &kiffalert.Handler{Accounts: accounts, Runs: runs, Log: log}}}
}

// only serves h at path, exactly, and 404 for any other path, cleaned or not.
type only struct {
	path string
	h    http.Handler
}

func (o only) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != o.path {
		http.NotFound(w, r)
		return
	}
	o.h.ServeHTTP(w, r)
}

// handle serves one request through the function URL. Any other event is
// refused: nothing else invokes this function.
func (f *function) handle(ctx context.Context, e events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
	if e.RequestContext.HTTP.Method == "" {
		return events.LambdaFunctionURLResponse{}, errors.New("kiffalerts: an event this function doesn't take")
	}
	r, err := lambdaurl.Request(ctx, e)
	if err != nil {
		return events.LambdaFunctionURLResponse{StatusCode: http.StatusBadRequest}, nil
	}
	w := httptest.NewRecorder()
	f.http.ServeHTTP(w, r)
	res := w.Result()
	body, _ := io.ReadAll(res.Body)
	out := events.LambdaFunctionURLResponse{StatusCode: res.StatusCode, Body: string(body), Headers: map[string]string{}}
	for k, v := range res.Header {
		out.Headers[k] = strings.Join(v, ", ")
	}
	return out, nil
}
