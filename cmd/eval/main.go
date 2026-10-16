// Command eval grades runs against the eval cases and prints a score and cost
// per case.
//
//	eval [-judge-model id -judge-price in,out] [-ids seed.json] [-baseline base.json] [-out results.json] case=session...
//	eval -run -model id -price in,out[,cache-read,cache-write] -jobs dir [-skill file] [-notes dir] [-gateway url] [-max-cost usd] [...] case...
//
// In the first form, each argument names a case (refunds, chargebacks) and the
// Managed Agents session of the run to grade, read with ANTHROPIC_API_KEY from
// the environment: the baseline.
//
// With -run, each argument names a case, and eval runs it on this runtime
// first: one run (trigger eval) of an agent whose instructions are the case's
// job, read from <jobs>/<job>.md, with -skill added, on the model given,
// through KIFF's gateway with the key in KIFF_AGENT_KEY. Tool calls and
// results are kept for grading (eval.Policy). Runs and sessions are kept in
// memory. With -notes, <notes>/<case>.md is the owner's note in the agent's
// memory before the run. A run KIFF holds a call of stops PAUSED, and is
// graded there.
//
// With -baseline, a case that scores lower than it did there fails the
// command. -ids is the seed's Stripe ids by item reference.
//
// With -judge-model, checks that need a judge ask that model on the Claude API,
// with the same key; its price is in millionths of a dollar per million input
// and output tokens. Without it, those checks are skipped and the output says
// which.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/kiff/agents/eval"
	"github.com/kiff/agents/eval/cases"
	"github.com/kiff/agents/eval/managed"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/model/anthropic"
	"github.com/kiff/agents/registry"
	runs "github.com/kiff/agents/run"
	"github.com/kiff/agents/runner"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/kiff"
	toolmemory "github.com/kiff/agents/tool/memory"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "eval:", err)
		os.Exit(1)
	}
}

// reader reads one session as a run.
type reader interface {
	Read(ctx context.Context, id string) (eval.Run, error)
}

var newReader = func(base, key string) reader { return managed.New(base, key, nil) }

var newModel = func(key string) model.Model { return anthropic.New(option.WithAPIKey(key)) }

// connect lists the agent's tools through KIFF's gateway.
var connect = func(ctx context.Context, gateway, key string) ([]tool.Tool, error) {
	return kiff.New(gateway, key, nil).Tools(ctx)
}

// evalTenant is the tenant every -run run is kept under, in memory.
const evalTenant = "eval"

// evalKeep is how long a -run run's events are kept: past the command.
const evalKeep = 24 * time.Hour

// runtime runs cases on this runtime, kept in memory.
type runtime struct {
	agents   *memory.Agents
	sessions *memory.Store
	runs     *memory.Runs
	rn       *runner.Runner
	base     registry.Definition
	jobs     string // the directory of the jobs' instructions
	notes    string // the directory of the owner's notes, <case>.md each; empty: none
	skill    string // added to every job's instructions
	log      io.Writer
}

// envKey is the agent's KIFF key, from the environment.
type envKey string

func (k envKey) Key(context.Context, string, string) (string, error) { return string(k), nil }

func newRuntime(m model.Model, kiffKey string, base registry.Definition, jobs, skill string, log io.Writer) *runtime {
	rt := &runtime{agents: memory.NewAgents(), sessions: memory.New(), runs: memory.NewRuns(), base: base, jobs: jobs, skill: skill, log: log}
	rt.rn = &runner.Runner{Runs: rt.runs, Sessions: rt.sessions, Collect: kiff.Collect, Builder: &registry.Builder{
		Agents: rt.agents, Secrets: envKey(kiffKey), Model: m, Gateways: []string{base.Gateway}, Connect: connect,
		Own: func(tenant, agent string) []tool.Tool { return []tool.Tool{toolmemory.New(rt.sessions, tenant, agent)} },
	}}
	return rt
}

// note puts the owner's note for the case, <notes>/<case>.md, in the agent's
// memory, as the owner would have left it: the agent reads it with its
// memory tool. A case with no note file starts with none.
func (rt *runtime) note(ctx context.Context, name string) error {
	if rt.notes == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(rt.notes, name+".md"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: the owner's note: %w", name, err)
	}
	return rt.sessions.PutNote(ctx, session.Note{Tenant: evalTenant, Agent: name, Name: "owner",
		Text: strings.TrimSpace(string(b)), Updated: time.Now().UTC()})
}

// cancel asks a run to stop, re-reading it if it changed meanwhile.
func (rt *runtime) cancel(tenant, id string) {
	ctx := context.Background()
	for try := 0; try < 5; try++ {
		r, err := rt.runs.Get(ctx, tenant, id)
		if err != nil || r.Status.Final() || r.Cancel {
			return
		}
		if _, err := runs.Change(ctx, rt.runs, r, time.Now(), func(n *runs.Run) error { n.Cancel = true; return nil }); !errors.Is(err, runs.ErrConflict) {
			return
		}
	}
}

// Run runs the case once, as a run of its job's agent, and reads it back for
// grading. A run that failed is graded as it stopped.
func (rt *runtime) Run(ctx context.Context, s cases.Scenario) (eval.Run, error) {
	if err := ctx.Err(); err != nil { // interrupted: start nothing more
		return eval.Run{}, fmt.Errorf("%s: not started: %w", s.Name, err)
	}
	job, err := os.ReadFile(filepath.Join(rt.jobs, s.Job+".md"))
	if err != nil {
		return eval.Run{}, fmt.Errorf("%s: the job's instructions: %w", s.Name, err)
	}
	d := rt.base
	d.ID, d.Instructions = s.Name, strings.TrimSpace(string(job))
	d.Keep = eval.Policy(evalKeep) // the checks read tool calls and results
	if rt.skill != "" {
		d.Instructions += "\n\n" + rt.skill
	}
	if err := rt.agents.Put(ctx, d); err != nil {
		return eval.Run{}, err
	}
	if err := rt.note(ctx, s.Name); err != nil {
		return eval.Run{}, err
	}
	r, err := rt.rn.Start(ctx, server.Start{Tenant: evalTenant, Agent: d.ID, Trigger: runs.ByEval, Input: s.Input})
	if err != nil {
		return eval.Run{}, fmt.Errorf("%s: %w", s.Name, err)
	}
	// The runner detaches a run from its caller's context, as a server run
	// outlives its request; an interrupt here asks the run to stop instead,
	// and it does at its next checkpoint.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-done:
		case <-ctx.Done():
			rt.cancel(r.Tenant, r.ID)
		}
	}()
	if _, err := rt.rn.Execute(ctx, evalTenant, r.ID, nil); err != nil {
		if _, gerr := rt.runs.Get(ctx, evalTenant, r.ID); gerr != nil {
			return eval.Run{}, fmt.Errorf("%s: %w", s.Name, err)
		}
		fmt.Fprintf(rt.log, "eval: %s: the run failed, graded as it stopped: %v\n", s.Name, err)
	}
	return eval.Load(ctx, rt.sessions, rt.runs, evalTenant, r.ID)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, env func(string) string) error {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	idsPath := fs.String("ids", "", "the seed's Stripe ids by item reference (JSON)")
	basePath := fs.String("baseline", "", "results to compare against (JSON); a lower score fails")
	outPath := fs.String("out", "", "also write the results here (JSON)")
	judgeModel := fs.String("judge-model", "", "the judge's model id; without it, judged checks are skipped")
	judgePrice := fs.String("judge-price", "", "the judge's input,output price in millionths of a dollar per million tokens")
	runHere := fs.Bool("run", false, "run each case on this runtime, then grade it; arguments are case names")
	modelID := fs.String("model", "", "-run: the agent's model id")
	price := fs.String("price", "", "-run: the agent's input,output[,cache-read,cache-write] price in millionths of a dollar per million tokens; with cache prices, turns use the prompt cache")
	jobs := fs.String("jobs", "", "-run: the directory of the jobs' instructions, <job>.md each")
	skillPath := fs.String("skill", "", "-run: a file with KIFF's skill, added to every job's instructions")
	notesDir := fs.String("notes", "", "-run: the directory of the owner's notes, <case>.md each, put in the agent's memory first")
	gateway := fs.String("gateway", kiff.URL, "-run: KIFF's gateway")
	maxCost := fs.Float64("max-cost", 2, "-run: the most one case's run may cost, in US dollars")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		if *runHere {
			return errors.New("name at least one case")
		}
		return errors.New("name at least one case=session")
	}

	scenarios := map[string]cases.Scenario{"refunds": cases.Refunds, "chargebacks": cases.Chargebacks}
	var ids map[string][]string
	if *idsPath != "" {
		if err := readJSON(*idsPath, &ids); err != nil {
			return err
		}
	}
	key := env("ANTHROPIC_API_KEY")
	if key == "" {
		return errors.New("set ANTHROPIC_API_KEY to read the runs")
	}
	src := newReader(env("ANTHROPIC_BASE_URL"), key)
	var rt *runtime
	if *runHere {
		kiffKey := env("KIFF_AGENT_KEY")
		switch {
		case kiffKey == "":
			return errors.New("set KIFF_AGENT_KEY to the agent's KIFF key for -run")
		case *modelID == "":
			return errors.New("-run needs -model")
		case *jobs == "":
			return errors.New("-run needs -jobs, the directory of the jobs' instructions")
		}
		p, err := parsePrice(*price)
		if err != nil {
			return fmt.Errorf("-price: %w", err)
		}
		var skill string
		if *skillPath != "" {
			b, err := os.ReadFile(*skillPath)
			if err != nil {
				return err
			}
			skill = strings.TrimSpace(string(b))
		}
		rt = newRuntime(newModel(key), kiffKey, registry.Definition{Tenant: evalTenant, Model: *modelID, Price: p,
			MaxTokens: 8192, Gateway: *gateway, KeyRef: "KIFF_AGENT_KEY",
			MaxCost: int64(*maxCost * 1_000_000), History: 20}, *jobs, skill, stderr)
		rt.notes = *notesDir
	}
	var judge *eval.Judge
	if *judgeModel != "" {
		p, err := parsePrice(*judgePrice)
		if err != nil {
			return fmt.Errorf("-judge-price: %w", err)
		}
		judge = &eval.Judge{Model: newModel(key), ModelID: *judgeModel, Price: p}
	}

	var results []eval.Result
	var skipped []string // cases an interrupt kept from starting
	for i, arg := range fs.Args() {
		if rt != nil && ctx.Err() != nil {
			skipped = fs.Args()[i:]
			break
		}
		var r eval.Run
		var s cases.Scenario
		if rt != nil {
			known := false
			if s, known = scenarios[arg]; !known {
				return fmt.Errorf("%q: want a case, refunds or chargebacks", arg)
			}
			var err error
			if r, err = rt.Run(ctx, s.WithIDs(ids)); err != nil {
				return err
			}
		} else {
			name, id, ok := strings.Cut(arg, "=")
			known := false
			s, known = scenarios[name]
			if !ok || !known || id == "" {
				return fmt.Errorf("%q: want case=session, with a case of refunds or chargebacks", arg)
			}
			var err error
			if r, err = src.Read(ctx, id); err != nil {
				return err
			}
		}
		res, err := s.WithIDs(ids).Case().Grade(ctx, r, judge)
		if err != nil {
			return err
		}
		results = append(results, res)
		fmt.Fprintf(stderr, "%s  %d/%d  $%.4f a run, $%.4f an item, judge $%.4f  (%s)\n",
			res.Case, res.Passed, res.Graded, dollars(res.CostMicroUSD), dollars(res.PerItemMicro), dollars(res.JudgeMicroUSD), res.Session)
		if cx := res.Context; cx.Turns > 0 {
			room := "unknown"
			if cx.LeastRoomTokens >= 0 {
				room = fmt.Sprintf("%d", cx.LeastRoomTokens)
			}
			fmt.Fprintf(stderr, "  context: %d turns, %d input tokens, %.0f%% from the cache, peak turn %d, least room %s\n",
				cx.Turns, cx.PromptTokens, 100*cx.CacheShare, cx.PeakPromptTokens, room)
		}
		for _, c := range res.Checks {
			mark := "pass"
			switch {
			case c.Skipped:
				mark = "skip"
			case !c.Pass:
				mark = "FAIL"
			}
			fmt.Fprintf(stderr, "  %s  %s", mark, c.Name)
			if c.Why != "" {
				fmt.Fprintf(stderr, ": %s", c.Why)
			}
			fmt.Fprintln(stderr)
		}
	}

	out, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if _, err := stdout.Write(out); err != nil {
		return err
	}
	if *outPath != "" {
		if err := os.WriteFile(*outPath, out, 0o644); err != nil {
			return err
		}
	}
	if len(skipped) > 0 {
		return fmt.Errorf("interrupted: not started: %s", strings.Join(skipped, ", "))
	}
	if *basePath != "" {
		var base []eval.Result
		if err := readJSON(*basePath, &base); err != nil {
			return err
		}
		if lower := eval.Lower(base, results); len(lower) > 0 {
			return fmt.Errorf("lower than the baseline: %s", strings.Join(lower, "; "))
		}
	}
	return nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func parsePrice(s string) (model.Price, error) {
	parts := strings.Split(s, ",")
	n := make([]int64, len(parts))
	for i, part := range parts {
		v, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || v <= 0 {
			n = nil
			break
		}
		n[i] = v
	}
	switch len(n) {
	case 2:
		return model.Price{Input: n[0], Output: n[1]}, nil
	case 4: // with cache prices, the turns ask for the cache
		return model.Price{Input: n[0], Output: n[1], CacheRead: n[2], CacheWrite: n[3]}, nil
	}
	return model.Price{}, errors.New("a price is input,output[,cache-read,cache-write] in millionths of a dollar per million tokens, all positive")
}

func dollars(micro int64) float64 { return float64(micro) / 1e6 }
