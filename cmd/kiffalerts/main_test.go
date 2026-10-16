package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/kiff/agents/cmd/internal/background"
	"github.com/kiff/agents/registry/secretsmanager"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/server/kiffalert"
	"github.com/kiff/agents/store/memory"
)

var ctx = context.Background()

const signing = "whsec_test"

// secrets is Secrets Manager holding one KIFF account's secret.
type secrets struct{}

func (secrets) GetSecretValue(_ context.Context, in *sm.GetSecretValueInput, _ ...func(*sm.Options)) (*sm.GetSecretValueOutput, error) {
	if aws.ToString(in.SecretId) != "kiff-accounts/kiff-acct-1" {
		return nil, &types.ResourceNotFoundException{}
	}
	return &sm.GetSecretValueOutput{Name: in.SecretId,
		SecretString: aws.String(fmt.Sprintf(`{"tenant": "t1", "secret": %q}`, signing))}, nil
}

// invocations records each Invoke, and answers 202.
type invocations struct{ in []*lambdasvc.InvokeInput }

func (i *invocations) Invoke(_ context.Context, in *lambdasvc.InvokeInput, _ ...func(*lambdasvc.Options)) (*lambdasvc.InvokeOutput, error) {
	i.in = append(i.in, in)
	return &lambdasvc.InvokeOutput{StatusCode: 202}, nil
}

// newTest is the function on a memory store holding t1's run, paused on one
// call KIFF holds as exc_1.
func newTest(t *testing.T) (*function, *memory.Runs, *invocations, run.Run) {
	t.Helper()
	runs, now := memory.NewRuns(), time.Now().UTC()
	r := run.New("t1", "rae", "s1", run.ByOwner, "Refund the order.", now)
	if err := runs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	r, err := run.Claim(ctx, runs, "t1", r.ID, "i1", now.Add(time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	r, err = run.Save(ctx, runs, r, now, func(n *run.Run) error {
		n.Status = run.Paused
		n.Requirements = []run.Requirement{{CallID: "c1", Tool: "stripe_refund", Operation: "op-1", Exception: "exc_1",
			Expires: now.Add(10 * time.Minute)}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	inv := &invocations{}
	f := newFunction(secretsmanager.NewAccounts(secrets{}, "kiff-accounts/"), runs,
		background.Invoke(inv, "agents-fn"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	return f, runs, inv, r
}

// request is a function URL event, as Lambda sends one with AuthType NONE.
func request(method, path, body string, header map[string]string) events.LambdaFunctionURLRequest {
	e := events.LambdaFunctionURLRequest{RawPath: path, Body: body, Headers: header}
	e.RequestContext.HTTP.Method = method
	return e
}

// alert is a signed hold_answered alert for exc_1.
func alert(account, status string) (string, map[string]string) {
	body := fmt.Sprintf(`{"schema_version":"1","tenant_id":%q,"outcome":"hold_answered","hold":{"id":"exc_1","status":%q},`+
		`"decided_at":%q,"event_id":"evt_1"}`, account, status, time.Now().UTC().Format(time.RFC3339))
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := "t=" + ts + ",v1=" + hex.EncodeToString(kiffalert.Sign(signing, ts, []byte(body)))
	return body, map[string]string{"content-type": "application/json", strings.ToLower(kiffalert.SignatureHeader): sig}
}

func TestAnAnswerContinuesItsRunInTheAgentsFunction(t *testing.T) {
	f, runs, inv, r := newTest(t)
	body, header := alert("kiff-acct-1", "approved")
	res, err := f.handle(ctx, request(http.MethodPost, "/kiff/alerts", body, header))
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("%d %q (%v)", res.StatusCode, res.Body, err)
	}
	got, _ := runs.Get(ctx, "t1", r.ID)
	if got.Requirements[0].Answered.IsZero() {
		t.Fatal("the answer isn't recorded")
	}
	if len(inv.in) != 1 || aws.ToString(inv.in[0].FunctionName) != "agents-fn" || inv.in[0].InvocationType != "Event" {
		t.Fatalf("invocations %+v, want one asynchronous invoke of the agents function", inv.in)
	}
	var e background.Event
	if err := json.Unmarshal(inv.in[0].Payload, &e); err != nil || e.Run.Tenant != "t1" || e.Run.ID != r.ID {
		t.Fatalf("sent %s, want the run event", inv.in[0].Payload)
	}
}

// The URL takes anyone's post: an alert that isn't KIFF's changes nothing.
func TestAnAlertThatDoesntCheckChangesNothing(t *testing.T) {
	f, runs, inv, r := newTest(t)
	body, header := alert("kiff-acct-1", "approved")
	unsigned := map[string]string{"content-type": "application/json"}
	other, otherHeader := alert("kiff-acct-2", "approved")
	for name, e := range map[string]events.LambdaFunctionURLRequest{
		"unsigned":        request(http.MethodPost, "/kiff/alerts", body, unsigned),
		"altered":         request(http.MethodPost, "/kiff/alerts", strings.Replace(body, "approved", "refused", 1), header),
		"unknown account": request(http.MethodPost, "/kiff/alerts", other, otherHeader),
	} {
		res, err := f.handle(ctx, e)
		if err != nil || res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: %d (%v), want 401", name, res.StatusCode, err)
		}
	}
	if got, _ := runs.Get(ctx, "t1", r.ID); !got.Requirements[0].Answered.IsZero() || len(inv.in) != 0 {
		t.Fatalf("an alert that doesn't check answered %+v, invoked %d", got.Requirements[0], len(inv.in))
	}
}

// The function's URL has no IAM auth, so it serves the alerts and nothing
// else: no route of the agents' API is reachable through it.
func TestOnlyTheAlertsAreServed(t *testing.T) {
	f, _, inv, _ := newTest(t)
	for _, p := range []string{"/", "/health", "/agents", "/agents/rae/runs", "/kiff/alerts/x", "/kiff", "/kiff/alerts/../../agents", "//kiff/alerts", "/kiff/alerts/", "/KIFF/alerts"} {
		res, err := f.handle(ctx, request(http.MethodPost, p, "{}", nil))
		if err != nil || res.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s: %d (%v), want 404", p, res.StatusCode, err)
		}
	}
	if res, _ := f.handle(ctx, request(http.MethodGet, "/kiff/alerts", "", nil)); res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /kiff/alerts: %d, want 405", res.StatusCode)
	}
	if len(inv.in) != 0 {
		t.Fatal("invoked the agents function")
	}
}

func TestOtherEventsAreRefused(t *testing.T) {
	f, _, inv, _ := newTest(t)
	var e events.LambdaFunctionURLRequest
	_ = json.Unmarshal([]byte(`{"run":{"tenant":"t1","id":"x"}}`), &e)
	if _, err := f.handle(ctx, e); err == nil {
		t.Fatal("took a run event")
	}
	if len(inv.in) != 0 {
		t.Fatal("invoked the agents function")
	}
}

func TestSetupNeedsItsConfiguration(t *testing.T) {
	required := []string{"AGENTS_TABLE", "KIFF_ACCOUNTS_PREFIX", "AGENTS_SECRETS_PREFIX", "AGENTS_FUNCTION"}
	for _, k := range required {
		t.Setenv(k, "")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := setup(ctx, log)
	if err == nil {
		t.Fatal("set up with no configuration")
	}
	for _, k := range required {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("the error doesn't name %s: %v", k, err)
		}
	}
	t.Setenv("AGENTS_TABLE", "agents")
	t.Setenv("AGENTS_FUNCTION", "agents-fn")
	t.Setenv("AGENTS_SECRETS_PREFIX", "agents/")
	for _, bad := range []string{"agents/", "agents/kiff-accounts/", "agent"} {
		t.Setenv("KIFF_ACCOUNTS_PREFIX", bad)
		if _, err := setup(ctx, log); err == nil {
			t.Errorf("set up with KIFF_ACCOUNTS_PREFIX %q over the agents' keys", bad)
		}
	}
	t.Setenv("KIFF_ACCOUNTS_PREFIX", "kiff-accounts/")
	t.Setenv("AWS_REGION", "eu-west-3")
	f, err := setup(ctx, log)
	if err != nil {
		t.Fatalf("a complete configuration: %v", err)
	}
	// The alerts are served (a body that isn't an alert is refused before
	// anything is read), and nothing else.
	if res, _ := f.handle(ctx, request(http.MethodPost, "/kiff/alerts", "not json", nil)); res.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /kiff/alerts: %d, want 400", res.StatusCode)
	}
	if res, _ := f.handle(ctx, request(http.MethodPost, "/agents", "{}", nil)); res.StatusCode != http.StatusNotFound {
		t.Errorf("POST /agents: %d, want 404", res.StatusCode)
	}
}
