package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/runner"
	"github.com/kiff/agents/schedule"
	"github.com/kiff/agents/server"
)

// runEvent is what a background invocation is sent: the run to execute. The
// run is already stored PENDING; the event names it and carries nothing else,
// so a repeated or made-up event can only execute a run that exists, once.
type runEvent struct {
	Run struct {
		Tenant string `json:"tenant"`
		ID     string `json:"id"`
	} `json:"run"`
}

// invoker is the part of the Lambda client background uses.
type invoker interface {
	Invoke(context.Context, *lambdasvc.InvokeInput, ...func(*lambdasvc.Options)) (*lambdasvc.InvokeOutput, error)
}

// background has another invocation of function execute the run: an
// asynchronous invoke, which returns once Lambda has queued the event. The
// server marks the run ERROR when this fails, so nothing waits on a run no
// invocation will claim.
func background(api invoker, function string) func(ctx context.Context, tenant, id string) error {
	return func(ctx context.Context, tenant, id string) error {
		var e runEvent
		e.Run.Tenant, e.Run.ID = tenant, id
		payload, err := json.Marshal(e)
		if err != nil {
			return err
		}
		out, err := api.Invoke(ctx, &lambdasvc.InvokeInput{
			FunctionName:   &function,
			InvocationType: lambdatypes.InvocationTypeEvent,
			Payload:        payload,
		})
		if err != nil {
			return fmt.Errorf("lambda: queueing a background run: %w", err)
		}
		if out.StatusCode != 202 {
			return fmt.Errorf("lambda: queueing a background run: status %d", out.StatusCode)
		}
		return nil
	}
}

// execute runs a background run to its next stop. Whatever happens, the run's
// state is what the runner stored, so execute never fails the invocation:
// that would only have Lambda retry an event the runner already answered.
//   - A run already claimed, ended or gone (a repeated event) is left alone.
//   - A run that failed is stored ERROR by the runner; its cause is logged.
//   - A run that couldn't be claimed at all stays PENDING, and the sweep marks
//     it ERROR once nobody has claimed it in time.
func (f *function) execute(ctx context.Context, tenant, id string) error {
	if tenant == "" || id == "" {
		return errors.New("lambda: a run event names no run")
	}
	r, err := f.runner.Execute(ctx, tenant, id, nil)
	switch {
	case err == nil:
		f.log.InfoContext(ctx, "run", "tenant", tenant, "run", id, "status", r.Status, "stop", r.Stop)
	case errors.Is(err, run.ErrConflict), errors.Is(err, run.ErrTransition), errors.Is(err, run.ErrNotFound):
		f.log.WarnContext(ctx, "run not executed", "tenant", tenant, "run", id, "err", err)
	case errors.Is(err, runner.ErrNoCollect):
		f.log.WarnContext(ctx, "paused run can't continue yet", "tenant", tenant, "run", id)
	default:
		f.log.ErrorContext(ctx, "run", "tenant", tenant, "run", id, "status", r.Status, "err", err)
	}
	return nil
}

// fire starts the run a schedule's firing asks for, and executes it in this
// invocation, which EventBridge Scheduler already started in the background.
// The run's ID is the firing's key, so a repeated delivery finds the run
// stored (run.ErrExists) and starts no other. A stored run still PENDING is
// one whose first invocation died before claiming it, and it gets run here.
// Any other is left as it is: going, ended, or paused until KIFF answers,
// which is for the answer to continue, not a repeated firing.
func (f *function) fire(ctx context.Context, fi schedule.Firing) error {
	r, err := f.runner.Start(ctx, server.Start{Tenant: fi.Tenant, Agent: fi.Agent, Trigger: run.BySchedule,
		Source: fi.Schedule, Input: fi.Input, ID: fi.Key()})
	switch {
	case errors.Is(err, run.ErrExists):
		if r.Status != run.Pending {
			f.log.InfoContext(ctx, "firing repeated", "tenant", r.Tenant, "run", r.ID, "status", r.Status)
			return nil
		}
	case errors.Is(err, registry.ErrSwitching):
		// Skipped, not retried: a retry could start it on the version
		// the owner is leaving, or late on the next one (kiff/tafs#107).
		f.log.InfoContext(ctx, "firing skipped: the agent is switching versions", "tenant", fi.Tenant, "schedule", fi.Schedule, "agent", fi.Agent)
		return nil
	case errors.Is(err, registry.ErrHalted):
		// Skipped, not retried: a retry would run it once resumed, and
		// nothing from before a halt runs.
		f.log.InfoContext(ctx, "firing skipped: the agent is halted", "tenant", fi.Tenant, "schedule", fi.Schedule, "agent", fi.Agent)
		return nil
	case err != nil:
		return err
	}
	return f.execute(ctx, r.Tenant, r.ID)
}

// sweepLimit bounds the runs one sweep reads, of each kind. The sweep runs
// every minute, so this ends up to 100 runs a minute that nobody else is left
// to end, and continues up to 100 paused ones.
const sweepLimit = 100

// sweeping is the sweep the function runs on each sweep event: across tenants,
// it marks ended the runs nobody is left to end (run.Sweeper). Each run's
// recording is ended by the runner first, and its expiry comes from its
// agent's keep rules, read under the sweep's own context.
//
// A paused run whose held calls KIFF has all resolved (cmd/kiffalerts records
// the answers) is sent to cont, another invocation, as a background run is:
// one sweep doesn't wait on a hundred runs.
func sweeping(runs run.Runs, rn *runner.Runner, cont func(ctx context.Context, tenant, id string) error) func(ctx context.Context, now time.Time) (int, error) {
	return func(ctx context.Context, now time.Time) (int, error) {
		sw := run.Sweeper{Runs: runs, Ended: rn.Ended, Continue: cont,
			Expires: func(r run.Run) time.Time { return rn.Expires(ctx, r, now) }}
		return sw.Sweep(ctx, now, sweepLimit)
	}
}

// sweep handles a sweep event. A run it couldn't end stays due, and the next
// sweep tries again; the error fails this invocation so it shows.
func (f *function) sweep(ctx context.Context) error {
	now := time.Now().UTC()
	if f.now != nil {
		now = f.now().UTC()
	}
	n, err := f.sweeper(ctx, now)
	if err != nil {
		f.log.ErrorContext(ctx, "sweep", "ended", n, "err", err)
		return err
	}
	if n > 0 {
		f.log.InfoContext(ctx, "sweep", "ended", n)
	}
	// Attached files go when due: an upload no message named within an
	// hour, and a file whose message has gone (kiff/tafs#209). What's left
	// is removed by the next sweep.
	if f.files != nil {
		n, err := f.files.Sweep(ctx, sweepLimit)
		if err != nil {
			f.log.ErrorContext(ctx, "sweeping attachments", "removed", n, "err", err)
		} else if n > 0 {
			f.log.InfoContext(ctx, "sweeping attachments", "removed", n)
		}
	}
	// Trials idle for a week end too (kiff/tafs#173). One KIFF can't end now
	// is tried again on the next sweep, so its error doesn't fail this one.
	if f.api == nil {
		return nil
	}
	trials, err := f.api.SweepTrials(ctx)
	if err != nil {
		f.log.ErrorContext(ctx, "sweeping trials", "ended", len(trials), "err", err)
	}
	if len(trials) > 0 {
		f.log.InfoContext(ctx, "sweeping trials", "ended", len(trials))
	}
	return nil
}

// secretAPI is the part of the Secrets Manager client modelKey uses.
type secretAPI interface {
	GetSecretValue(context.Context, *sm.GetSecretValueInput, ...func(*sm.Options)) (*sm.GetSecretValueOutput, error)
}

// modelKey reads the model provider's key from the secret named name. The
// key is kept in memory only, and no error carries it.
func modelKey(ctx context.Context, api secretAPI, name string) (string, error) {
	out, err := api.GetSecretValue(ctx, &sm.GetSecretValueInput{SecretId: &name})
	if err != nil {
		return "", fmt.Errorf("lambda: reading the model key: %w", err)
	}
	if out.SecretString == nil || strings.TrimSpace(*out.SecretString) == "" {
		return "", errors.New("lambda: the model key's secret has no string")
	}
	return strings.TrimSpace(*out.SecretString), nil
}

// keyedModel is the model, made with its key the first time a turn needs it.
// Reading the key then, not at setup, keeps a cold start from reading a secret
// that only runs use, and setup from calling AWS. A failed read fails that
// turn, and the next turn reads again; a key once read is kept.
type keyedModel struct {
	key  func(ctx context.Context) (string, error)
	make func(key string) model.Model

	mu sync.Mutex
	m  model.Model
}

func (k *keyedModel) Turn(ctx context.Context, req model.Request, onText func(string)) (model.Reply, error) {
	m, err := k.model(ctx)
	if err != nil {
		return model.Reply{}, err
	}
	return m.Turn(ctx, req, onText)
}

// Window is the model's context window, for the run's context report
// (model.Windower), when the model knows it.
func (k *keyedModel) Window(ctx context.Context, id string) (int64, error) {
	m, err := k.model(ctx)
	if err != nil {
		return 0, err
	}
	w, ok := m.(model.Windower)
	if !ok {
		return 0, errors.New("lambda: the model knows no context window")
	}
	return w.Window(ctx, id)
}

// model is the model, made with its key on first use.
func (k *keyedModel) model(ctx context.Context) (model.Model, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.m == nil {
		key, err := k.key(ctx)
		if err != nil {
			return nil, err
		}
		k.m = k.make(key)
	}
	return k.m, nil
}

// outsidePrefix refuses a model key secret that an agent's key reference
// could name: everything under the agents' prefix is reachable as some
// tenant's key. The secret is given by name, so the check sees the name: an
// ARN would hide it.
func outsidePrefix(secret, prefix string) error {
	if strings.HasPrefix(secret, "arn:") {
		return errors.New("lambda: AGENTS_MODEL_KEY_SECRET is the secret's name, not its ARN")
	}
	if strings.HasPrefix(secret, prefix) {
		return fmt.Errorf("lambda: AGENTS_MODEL_KEY_SECRET must not be under AGENTS_SECRETS_PREFIX %q: an agent's key reference could name it", prefix)
	}
	return nil
}
