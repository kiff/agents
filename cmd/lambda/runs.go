package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/runner"
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
func (f *function) execute(ctx context.Context, e runEvent) error {
	tenant, id := e.Run.Tenant, e.Run.ID
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
	k.mu.Lock()
	m := k.m
	if m == nil {
		key, err := k.key(ctx)
		if err != nil {
			k.mu.Unlock()
			return model.Reply{}, err
		}
		k.m = k.make(key)
		m = k.m
	}
	k.mu.Unlock()
	return m.Turn(ctx, req, onText)
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
