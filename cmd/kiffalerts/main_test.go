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
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

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

// newTest is the function on a memory store holding t1's run, paused on one
// call KIFF holds as exc_1.
func newTest(t *testing.T) (*function, *memory.Runs, run.Run) {
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
	f := newFunction(secretsmanager.NewAccounts(secrets{}, "kiff-accounts/"), runs,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return f, runs, r
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

// An answer is recorded, and that is all: the run is left PAUSED, due to the
// agents function's sweep, which continues it.
func TestAnAnswerIsRecordedForTheSweep(t *testing.T) {
	f, runs, r := newTest(t)
	body, header := alert("kiff-acct-1", "approved")
	res, err := f.handle(ctx, request(http.MethodPost, "/kiff/alerts", body, header))
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("%d %q (%v)", res.StatusCode, res.Body, err)
	}
	got, _ := runs.Get(ctx, "t1", r.ID)
	if got.Status != run.Paused || got.Requirements[0].Answer != "approved" || got.Due().IsZero() {
		t.Fatalf("stored %+v, want the answer recorded and the run PAUSED, due", got)
	}
	due, err := runs.DuePaused(ctx, got.Due().Add(time.Second), 10)
	if err != nil || len(due) != 1 || due[0].ID != r.ID {
		t.Fatalf("due to the sweep: %+v (%v), want the run", due, err)
	}
}

// The URL takes anyone's post: an alert that isn't KIFF's changes nothing.
func TestAnAlertThatDoesntCheckChangesNothing(t *testing.T) {
	f, runs, r := newTest(t)
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
	if got, _ := runs.Get(ctx, "t1", r.ID); got.Version != r.Version {
		t.Fatalf("an alert that doesn't check changed the run: %+v", got.Requirements[0])
	}
}

// The function's URL has no IAM auth, so it serves the alerts and nothing
// else: no route of the agents' API is reachable through it.
func TestOnlyTheAlertsAreServed(t *testing.T) {
	f, runs, r := newTest(t)
	for _, p := range []string{"/", "/health", "/agents", "/agents/rae/runs", "/kiff/alerts/x", "/kiff", "/kiff/alerts/../../agents", "//kiff/alerts", "/kiff/alerts/", "/KIFF/alerts"} {
		res, err := f.handle(ctx, request(http.MethodPost, p, "{}", nil))
		if err != nil || res.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s: %d (%v), want 404", p, res.StatusCode, err)
		}
	}
	if res, _ := f.handle(ctx, request(http.MethodGet, "/kiff/alerts", "", nil)); res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /kiff/alerts: %d, want 405", res.StatusCode)
	}
	if got, _ := runs.Get(ctx, "t1", r.ID); got.Version != r.Version {
		t.Fatal("a request to another path changed the run")
	}
}

func TestOtherEventsAreRefused(t *testing.T) {
	f, _, _ := newTest(t)
	var e events.LambdaFunctionURLRequest
	_ = json.Unmarshal([]byte(`{"run":{"tenant":"t1","id":"x"}}`), &e)
	if _, err := f.handle(ctx, e); err == nil {
		t.Fatal("took a run event")
	}
}

func TestSetupNeedsItsConfiguration(t *testing.T) {
	required := []string{"AGENTS_TABLE", "KIFF_ACCOUNTS_PREFIX", "AGENTS_SECRETS_PREFIX"}
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
