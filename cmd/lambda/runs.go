package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/runner"
	"github.com/kiff/agents/schedule"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/tool"
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
	f.executed(ctx, tenant, id)
	return nil
}

// executed executes the run, logs how it went, frees its agent's waiting
// firing once it's no longer at work, and returns the run as it stands.
func (f *function) executed(ctx context.Context, tenant, id string) run.Run {
	r, err := f.runner.Execute(ctx, tenant, id, nil)
	if r.Agent != "" && !r.Active() && f.schedules != nil {
		// Its agent may be free now: a firing kept waiting for it runs.
		if err := f.schedules.Free(ctx, tenant, r.Agent); err != nil {
			f.log.ErrorContext(ctx, "freeing the agent's waiting firing", "tenant", tenant, "agent", r.Agent, "err", err)
		}
	}
	switch {
	case err == nil:
		f.log.InfoContext(ctx, "run", "tenant", tenant, "run", id, "status", r.Status, "stop", r.Stop)
	case errors.Is(err, run.ErrConflict), errors.Is(err, run.ErrTransition), errors.Is(err, run.ErrNotFound):
		f.log.WarnContext(ctx, "run not executed", "tenant", tenant, "run", id, "err", err)
	case errors.Is(err, runner.ErrNoCollect):
		f.log.WarnContext(ctx, "paused run can't continue yet", "tenant", tenant, "run", id)
	case errors.Is(err, run.ErrQueued):
		f.log.InfoContext(ctx, "run waits for its thread", "tenant", tenant, "run", id)
	default:
		f.log.ErrorContext(ctx, "run", "tenant", tenant, "run", id, "status", r.Status, "err", err)
	}
	return r
}

// unworked reports whether r failed before any turn: the model refused its
// first call (for want of credits, say), or it stopped before its loop. It
// did nothing, so a firing that handed it items hasn't used them up. Only a
// run never carried on by the sweep, with an error the runner stored itself:
// a run the sweep ended may have acted with its turns not yet stored.
func unworked(r run.Run) bool {
	return r.Status == run.Error && r.Turns == 0 && r.Resumes == 0 && runner.Own(r.Error)
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
		Source: fi.Schedule, Input: fi.Input + f.lastWorked(ctx, fi), ID: fi.Key()})
	var credits *server.CreditsRefusal
	switch {
	case errors.Is(err, run.ErrExists):
		if r.Status != run.Pending {
			f.log.InfoContext(ctx, "firing repeated", "tenant", r.Tenant, "run", r.ID, "status", r.Status)
			// The run a first delivery started did nothing: a repeat uses up nothing either.
			if unworked(r) && r.Error == runner.FailedSwitching {
				return schedule.ErrSwitching
			}
			if unworked(r) {
				return schedule.ErrUnworked
			}
			return nil
		}
	case errors.Is(err, registry.ErrSwitching):
		// Skipped, not retried: a retry could start it on the version
		// the owner is leaving, or late on the next one (kiff/tafs#107).
		// The schedule keeps it as skipped_switching, never as a run: no
		// run was made, and a check's cursor stays for the new version.
		f.log.InfoContext(ctx, "firing skipped: the agent is switching versions", "tenant", fi.Tenant, "schedule", fi.Schedule, "agent", fi.Agent)
		return schedule.ErrSwitching
	case errors.Is(err, registry.ErrHalted):
		// Skipped, not retried: a retry would run it once resumed, and
		// nothing from before a halt runs. The schedule keeps it as
		// skipped_halted, never as a run.
		f.log.InfoContext(ctx, "firing skipped: the agent is halted", "tenant", fi.Tenant, "schedule", fi.Schedule, "agent", fi.Agent)
		return schedule.ErrHalted
	case errors.As(err, &credits):
		// Skipped, not retried: KIFF answered, and the account's tafs wait
		// for credits. What the firing was handed waits for the next one
		// that runs (skipped_credits). KIFF not answering is an error, and
		// the firing is tried again.
		f.log.InfoContext(ctx, "firing skipped: the account's tafs wait for credits", "tenant", fi.Tenant, "schedule", fi.Schedule, "agent", fi.Agent, "why", credits.Why)
		return &schedule.CreditsError{Why: credits.Why}
	case err != nil:
		return err
	}
	// A run that failed before any turn did nothing: the firing says so, and
	// what it handed over goes back (schedule.FiredUnworked). One stopped
	// before it began because a switch began meanwhile is a switch's skip,
	// not a failure: it never counts toward failing.
	done := f.executed(ctx, r.Tenant, r.ID)
	if unworked(done) && done.Error == runner.FailedSwitching {
		return schedule.ErrSwitching
	}
	if unworked(done) {
		return schedule.ErrUnworked
	}
	return nil
}

// checkRead is a schedule's check read: read, and when it fails because the
// agent is halted or switching versions (startable says which), that skip
// (schedule.ErrHalted, ErrSwitching), never a read that failed, so neither a
// halt nor a switch counts toward the schedule's failing. A read that worked
// meanwhile goes on as before: its run is refused, and the schedule keeps the
// halt (the cursor moves past) or the switch (it stays).
func checkRead(startable func(ctx context.Context, tenant, agent string) error,
	read func(ctx context.Context, tenant, agent string, c schedule.Check) (string, error)) func(ctx context.Context, tenant, agent string, c schedule.Check) (string, error) {
	return func(ctx context.Context, tenant, agent string, c schedule.Check) (string, error) {
		result, err := read(ctx, tenant, agent, c)
		if err == nil {
			return result, nil
		}
		switch st := startable(ctx, tenant, agent); {
		case errors.Is(st, registry.ErrHalted):
			return "", schedule.ErrHalted
		case errors.Is(st, registry.ErrSwitching):
			return "", schedule.ErrSwitching
		}
		return "", err
	}
}

// lastLook is how many of a schedule's newest runs lastWorked reads.
const lastLook = 10

// lastWorked is what a firing's run is told of its schedule's last run that
// did any work, from the runtime and not from its notes: when it ended and how
// (answered; stopped at its turn or cost cap, so cut short; out of room; a
// declined turn; failed; cancelled), with the start of its last answer. A run
// still at work or paused for its owner hasn't ended, and is passed over. None
// among the newest: it's told so, so it never guesses where the last one
// stopped. Empty when the runs can't be read: the firing goes on without it.
func (f *function) lastWorked(ctx context.Context, fi schedule.Firing) string {
	if f.runs == nil {
		return ""
	}
	recent, err := f.runs.ListSource(ctx, fi.Tenant, run.BySchedule, fi.Schedule, lastLook)
	if err != nil {
		f.log.WarnContext(ctx, "reading the schedule's last runs", "tenant", fi.Tenant, "schedule", fi.Schedule, "err", err)
		return ""
	}
	for _, r := range recent {
		if r.ID == fi.Key() || r.Turns == 0 || (r.Status != run.Completed && r.Status != run.Error && r.Status != run.Cancelled) {
			continue
		}
		return handOver(r)
	}
	return "\n\n[From the runtime, not your notes: this schedule has no earlier run that did any work.]"
}

// handOver is the line lastWorked adds for r.
func handOver(r run.Run) string {
	how := "it answered and finished"
	switch {
	case r.Status == run.Error:
		how = "it failed after doing some work"
	case r.Status == run.Cancelled:
		how = "it was stopped by its owner before it finished"
	case r.Stop == "turn_cap":
		how = "it stopped at its turn limit, cut short before it finished"
	case r.Stop == "cost_cap":
		how = "it stopped at its cost limit, cut short before it finished"
	case r.Stop == "max_token":
		how = "it ran out of room mid-answer, cut short"
	case r.Stop == "refused":
		how = "a turn was declined, so it stopped before it finished"
	case r.Stop != "" && r.Stop != "answered":
		how = "it stopped (" + r.Stop + ") before it finished"
	}
	line := "\n\n[From the runtime, not your notes: your last run on this schedule that did any work ended " +
		endedAt(r).UTC().Format("2006-01-02 15:04 UTC") + "; " + how + "."
	if out := strings.Join(strings.Fields(r.Output), " "); out != "" {
		if len(out) > handOverOutput {
			cut := handOverOutput
			for cut > 0 && !utf8.RuneStart(out[cut]) {
				cut--
			}
			out = out[:cut] + "…"
		}
		line += " Its last answer began, in its own words, not the runtime's: \"" + out + "\""
	}
	return line + "]"
}

// endedAt is when r ended: its Ended, or its last change for a run stored
// before Ended was kept.
func endedAt(r run.Run) time.Time {
	if !r.Ended.IsZero() {
		return r.Ended
	}
	return r.Updated
}

// handOverOutput bounds how much of the last run's answer handOver quotes, in
// bytes.
const handOverOutput = 400

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
		sw := run.Sweeper{Runs: runs, Ended: rn.Ended, Continue: cont, Hold: rn.Held,
			// A note tafs cloud didn't take is sent again (run.NoteWaits), when runs keeps notes,
			// and so is a usage report KIFF didn't take (run.UsageWaits).
			Note:    noteAgain(runs),
			Usage:   usageAgain(runs),
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
	// A waiting firing whose agent's run ended where nobody freed it (a
	// chat, a run this sweep ended) runs now; one still busy keeps waiting.
	if f.schedules != nil {
		if err := f.schedules.FreeAll(ctx, sweepLimit); err != nil {
			f.log.ErrorContext(ctx, "freeing waiting firings", "err", err)
		}
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

// noteAgain is how the sweep sends a kept note again: Notifying's, when runs
// announces runs (under Reporting, or not), else none.
func noteAgain(runs run.Runs) func(ctx context.Context, r run.Run) error {
	if rp, ok := runs.(run.Reporting); ok {
		runs = rp.Runs
	}
	if n, ok := runs.(run.Notifying); ok {
		return n.Again
	}
	return nil
}

// usageAgain is how the sweep sends a kept usage report again: Reporting's,
// when runs reports runs, else none.
func usageAgain(runs run.Runs) func(ctx context.Context, r run.Run) error {
	if rp, ok := runs.(run.Reporting); ok {
		return rp.Again
	}
	return nil
}

// agentAtWork reports whether the firing's agent is still at work on a run
// other than the firing's own (a repeated delivery): PENDING or RUNNING,
// whatever started it (busy reads the taf, not one trigger). A
// run paused for its owner isn't at work, and doesn't keep the firing from
// running.
func agentAtWork(runs run.Runs) func(ctx context.Context, f schedule.Firing) (bool, error) {
	return func(ctx context.Context, f schedule.Firing) (bool, error) {
		recent, err := runs.List(ctx, f.Tenant, f.Agent, busyLook)
		if err != nil {
			return false, err
		}
		for _, r := range recent {
			if r.ID != f.Key() && r.Active() {
				return true, nil
			}
		}
		return false, nil
	}
}

// busyLook is how many of an agent's newest runs agentAtWork reads: a run at
// work is among the newest, since the tenant's runs going at once are capped.
const busyLook = 20

// probe makes a check's read: the agent's tool named c.Tool, called with
// c.Arguments through its gateway, which records and counts it on the
// agent's Card like any read. A tool that isn't a read, or a read KIFF
// didn't allow, is an error the firing reports, never a run.
func probe(ctx context.Context, b *registry.Builder, tenant, agent string, c schedule.Check) (string, error) {
	a, _, err := b.Build(ctx, tenant, agent)
	if err != nil {
		return "", err
	}
	for _, t := range a.Tools {
		if t.Spec().Name != c.Tool {
			continue
		}
		if t.Access() != tool.Read || !t.Gateway() {
			return "", fmt.Errorf("lambda: the check's tool %s isn't a read through the gateway", c.Tool)
		}
		in := c.Arguments
		if len(in) == 0 {
			in = json.RawMessage(`{}`)
		}
		res, err := t.Call(ctx, in)
		if err != nil {
			return "", err
		}
		if res.IsError || res.State != "" {
			return "", fmt.Errorf("lambda: the check's read %s wasn't made: %s", c.Tool, res.State)
		}
		return res.Text, nil
	}
	return "", fmt.Errorf("lambda: the agent has no tool %s to check with", c.Tool)
}
