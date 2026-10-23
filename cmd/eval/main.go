// Command eval grades runs against the eval cases and prints a score and cost
// per case.
//
//	eval [-judge-model id -judge-price in,out] [-ids seed.json] [-baseline base.json] [-out results.json] case=session...
//	eval -run -model id -price in,out[,cache-read,cache-write] -jobs dir [-skill file] [-notes dir] [-gateway url] [-max-cost usd] [...] case...
//	eval -run -definitions dir [-notes dir] [-gateway url] [-max-cost usd] [...] case...
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
// With -definitions instead of -model, -price, -jobs and -skill, each job's
// agent is <definitions>/<job>.json: the agent as kiff/tafs defines it
// (infra/eval-definitions.sh writes them from a tafs commit), so the eval
// runs the model, price, effort and instructions tafs runs. The eval keeps
// its own tenant, gateway, key, cost cap and keep rules.
//
// With -baseline, a case that scores lower than it did there fails the
// command. -ids is the seed's Stripe ids by item reference.
//
// With -judge-model, checks that need a judge ask that model on the Claude API,
// with the same key; its price is in millionths of a dollar per million input
// and output tokens. Without it, those checks are skipped and the output says
// which.
//
//	eval seed [-out seed.json] chargebacks
//
// fills the Stripe test-mode account of STRIPE_API_KEY with a case's items,
// after closing what an earlier seed left open, and writes their Stripe ids
// by reference: the file -ids reads. It also prints the owner's note the
// case's taf starts with. A live key is refused.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/kiff/agents/eval"
	"github.com/kiff/agents/eval/cases"
	"github.com/kiff/agents/eval/managed"
	"github.com/kiff/agents/eval/seed"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/model/anthropic"
	"github.com/kiff/agents/registry"
	runs "github.com/kiff/agents/run"
	"github.com/kiff/agents/runner"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/skill"
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

// withdraw withdraws one of the agent's held calls at KIFF.
var withdraw = func(ctx context.Context, apiURL, key, exception string) (kiff.Withdrawal, error) {
	return kiff.Withdraw(ctx, nil, apiURL, key, exception)
}

// cardRoom reads what the agent's Cards have left, through KIFF's gateway.
var cardRoom = func(ctx context.Context, gateway, key string) ([]kiff.Room, error) {
	return kiff.New(gateway, key, nil).CardRoom(ctx)
}

// errNotGraded means a run can't be graded fairly, so it isn't run.
var errNotGraded = errors.New("not graded")

// checkRoom refuses runs the agent's Cards can't fund (#78): with less room
// than the case needs, a run would measure the Card's day so far, not the
// agent. It checks runs of them at once, before the first: a night is funded
// in full or doesn't start, so no graded run's results are lost to a later
// one that couldn't run. Every limit covering an action must have the room.
func (rt *runtime) checkRoom(ctx context.Context, s cases.Scenario, runs int) error {
	if len(s.Room) == 0 {
		return nil
	}
	rooms, err := cardRoom(ctx, rt.base.Gateway, rt.kiffKey)
	if err != nil {
		return fmt.Errorf("%s: reading the Cards' room: %w", s.Name, err)
	}
	actions := make([]string, 0, len(s.Room))
	for a := range s.Room {
		actions = append(actions, a)
	}
	slices.Sort(actions)
	for _, action := range actions {
		need, found := s.Room[action]*int64(runs), false
		for _, r := range rooms {
			if r.Action != action {
				continue
			}
			found = true
			if r.Remaining < need {
				resets := ""
				if !r.ResetsAt.IsZero() {
					resets = ", resets at " + r.ResetsAt.Format(time.RFC3339)
				}
				return fmt.Errorf("%s: %w: %s has %d of %d left on its Card (%s %s%s); %d runs need %d",
					s.Name, errNotGraded, action, r.Remaining, r.Limit, r.Quantity, r.Window, resets, runs, need)
			}
		}
		if !found {
			return fmt.Errorf("%s: %w: no Card of the agent's covers %s", s.Name, errNotGraded, action)
		}
	}
	return nil
}

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
	skills   *memory.Skills // the packages -skills loaded, which definitions may name (#101)
	sessions *memory.Store
	runs     *memory.Runs
	rn       *runner.Runner
	base     registry.Definition
	jobs     string            // the directory of the jobs' instructions
	defs     string            // the directory of the agents' definitions, <job>.json each; empty: base, jobs and skill
	notes    string            // the directory of the owner's notes, <case>.md each; empty: none
	skill    string            // added to every job's instructions
	source   string            // where the definitions came from, for the report
	tools    []string          // the tools KIFF's gateway listed, last time
	configs  []eval.Config     // what each job's agent ran as, in the order jobs first ran
	notesFor map[string]string // the owner's note for a case, when no -notes file gives one
	kiffKey  string            // the agent's KIFF key
	kiffAPI  string            // KIFF's API, where held calls are withdrawn
	log      io.Writer
	public   bool // the log is public (-public): no ids, no run's details
}

// envKey is the agent's KIFF key, from the environment.
type envKey string

func (k envKey) Key(context.Context, string, string) (string, error) { return string(k), nil }

func newRuntime(m model.Model, kiffKey string, base registry.Definition, jobs, skill string, log io.Writer) *runtime {
	rt := &runtime{agents: memory.NewAgents(), sessions: memory.New(), runs: memory.NewRuns(), base: base, jobs: jobs, skill: skill,
		kiffKey: kiffKey, kiffAPI: kiff.APIURL, log: log}
	rt.skills = memory.NewSkills()
	rt.rn = &runner.Runner{Runs: rt.runs, Sessions: rt.sessions, Collect: kiff.Collect, Builder: &registry.Builder{
		Agents: rt.agents, Secrets: envKey(kiffKey), Model: m, Gateways: []string{base.Gateway}, Connect: rt.connect, Skills: rt.skills,
		Own: func(tenant, agent string) []tool.Tool { return []tool.Tool{toolmemory.New(rt.sessions, tenant, agent)} },
	}}
	return rt
}

// connect lists the agent's tools, and keeps their names for the report.
func (rt *runtime) connect(ctx context.Context, gateway, key string) ([]tool.Tool, error) {
	ts, err := connect(ctx, gateway, key)
	if err == nil {
		rt.tools = rt.tools[:0]
		for _, t := range ts {
			rt.tools = append(rt.tools, t.Spec().Name)
		}
	}
	return ts, err
}

// config records what the job's agent ran as, the first time it runs.
func (rt *runtime) config(job string, d registry.Definition) {
	for _, c := range rt.configs {
		if c.Job == job {
			return
		}
	}
	p := d.Price
	price := fmt.Sprintf("%d,%d", p.Input, p.Output)
	if p.Caches() {
		price += fmt.Sprintf(",%d,%d", p.CacheRead, p.CacheWrite)
	}
	sum := sha256.Sum256([]byte(d.Instructions))
	rt.configs = append(rt.configs, eval.Config{Job: job, Source: rt.source, Model: d.Model, Effort: d.Effort, MaxTokens: d.MaxTokens,
		Price: price, Instructions: fmt.Sprintf("%d bytes, sha256 %x", len(d.Instructions), sum[:6]), Tools: slices.Clone(rt.tools), Skills: refs(d.Skills)})
}

// note starts the agent's memory over with only the owner's note for the
// case, <notes>/<case>.md, as the owner would have left it: the agent reads
// it with its memory tool. A case with no note file starts with none. What an
// earlier run wrote is forgotten, so each run starts from the same fixture.
func (rt *runtime) note(ctx context.Context, name string) error {
	if err := rt.sessions.DeleteNotes(ctx, evalTenant, name); err != nil {
		return fmt.Errorf("%s: the agent's memory: %w", name, err)
	}
	text := rt.notesFor[name]
	if rt.notes != "" {
		b, err := os.ReadFile(filepath.Join(rt.notes, name+".md"))
		switch {
		case err == nil:
			text = string(b)
		case !errors.Is(err, os.ErrNotExist):
			return fmt.Errorf("%s: the owner's note: %w", name, err)
		}
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return rt.sessions.PutNote(ctx, session.Note{Tenant: evalTenant, Agent: name, Name: "owner",
		Text: strings.TrimSpace(text), Updated: time.Now().UTC()})
}

// definition is the agent a case runs as. From -definitions, it is the
// job's agent as kiff/tafs defines it: its model, price, effort, max tokens,
// max turns, history (by count and size) and instructions (with KIFF's skill). Otherwise it is the base
// with the job's instructions and the skill. Either way the eval keeps its
// own tenant, gateway, key, cost cap and keep rules: the checks read tool
// calls and results.
func (rt *runtime) definition(s cases.Scenario) (registry.Definition, error) {
	d := rt.base
	if rt.defs != "" {
		b, err := os.ReadFile(filepath.Join(rt.defs, s.Job+".json"))
		if err != nil {
			return d, fmt.Errorf("%s: the job's definition: %w", s.Name, err)
		}
		var t registry.Definition
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields() // as the server reads it: a field this runtime doesn't know fails here, not quietly
		if err := dec.Decode(&t); err != nil {
			return d, fmt.Errorf("%s: the job's definition: %w", s.Name, err)
		}
		if t.Workspace { // the eval's runner has none: refused, not run without it
			return d, fmt.Errorf("%s: the job's agent has a workspace, and the eval gives none yet", s.Name)
		}
		d.Model, d.Price, d.Effort, d.MaxTokens, d.MaxTurns, d.History, d.HistoryTokens, d.Instructions = t.Model, t.Price, t.Effort, t.MaxTokens, t.MaxTurns, t.History, t.HistoryTokens, t.Instructions
		d.Skills = t.Skills // read from -skills: a skill it names that isn't there fails the build
		d.DeferredTools = t.DeferredTools
	} else {
		job, err := os.ReadFile(filepath.Join(rt.jobs, s.Job+".md"))
		if err != nil {
			return d, fmt.Errorf("%s: the job's instructions: %w", s.Name, err)
		}
		d.Instructions = strings.TrimSpace(string(job))
		if rt.skill != "" {
			d.Instructions += "\n\n" + rt.skill
		}
	}
	d.ID = s.Name
	d.Keep = eval.Policy(evalKeep)
	return d, nil
}

func refs(rs []skill.Ref) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.String())
	}
	return out
}

// loadSkills stores every skill under dir for the eval's tenant (#101):
// <dir>/<name>/<version>/SKILL.md, and the files beside it, at any depth.
func (rt *runtime) loadSkills(ctx context.Context, dir string) error {
	versions, err := filepath.Glob(filepath.Join(dir, "*", "*", "SKILL.md"))
	if err != nil {
		return err
	}
	if len(versions) == 0 {
		return fmt.Errorf("-skills: no <name>/<version>/SKILL.md under %s", dir)
	}
	for _, path := range versions {
		root := filepath.Dir(path)
		md, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name, description, body, err := skill.Parse(string(md))
		if err != nil {
			return fmt.Errorf("-skills: %s: %w", path, err)
		}
		if name != filepath.Base(filepath.Dir(root)) {
			return fmt.Errorf("-skills: %s names %q: its directory must be named so", path, name)
		}
		files := map[string]string{}
		err = filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() || p == path {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			files[filepath.ToSlash(rel)] = string(b)
			return nil
		})
		if err != nil {
			return err
		}
		p := skill.Package{Tenant: evalTenant, Name: name, Version: filepath.Base(root), Description: description, Body: body}
		if len(files) > 0 {
			p.Files = files
		}
		if err := p.Check(); err != nil {
			return fmt.Errorf("-skills: %s: %w", root, err)
		}
		if err := rt.skills.Put(ctx, p); err != nil {
			return err
		}
	}
	return nil
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
	d, err := rt.definition(s)
	if err != nil {
		return eval.Run{}, err
	}
	if err := rt.agents.Put(ctx, d); err != nil {
		return eval.Run{}, err
	}
	defer func() { rt.config(s.Job, d) }() // after the run connected, so the tools are known
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
		fmt.Fprintf(rt.log, "eval: %s: the run failed, graded as it stopped: %v\n", s.Name, rt.say(err))
	}
	rt.withdrawHeld(s.Name, r.ID)
	return eval.Load(ctx, rt.sessions, rt.runs, evalTenant, r.ID)
}

// id is an id as the log shows it: hidden when the log is public.
func (rt *runtime) id(s string) string {
	if rt.public {
		return "…"
	}
	return s
}

// say is an error as the log shows it: its ids hidden when the log is public.
func (rt *runtime) say(err error) string {
	if rt.public {
		return redactIDs(err.Error())
	}
	return err.Error()
}

// idPattern matches the ids a run's log could carry: Stripe's (dp_, ch_,
// pi_, cus_, acct_, re_, …), KIFF's (exc_, …) and this runtime's (sesn_,
// run_). A public log shows none of them.
var idPattern = regexp.MustCompile(`\b[a-z]{2,5}_[A-Za-z0-9]{6,}\b`)

func redactIDs(s string) string { return idPattern.ReplaceAllString(s, "…") }

var stripeURL = ""

// withdrawHeld withdraws at KIFF every call the run left held, so none
// waits on the owner or carries into the next run: an eval run is graded
// where it paused, and never continued. It runs after an interrupt too, and
// fails nothing: a hold it can't withdraw is logged for the owner to refuse.
func (rt *runtime) withdrawHeld(name, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := rt.runs.Get(ctx, evalTenant, id)
	if err != nil {
		return
	}
	for _, q := range r.Requirements {
		if q.Exception == "" {
			fmt.Fprintf(rt.log, "eval: %s: KIFF held %s with no exception to withdraw: refuse it in KIFF\n", name, q.Tool)
			continue
		}
		w, err := withdraw(ctx, rt.kiffAPI, rt.kiffKey, q.Exception)
		switch {
		case errors.Is(err, kiff.ErrNoHold):
			fmt.Fprintf(rt.log, "eval: %s: KIFF no longer holds %s (%s)\n", name, q.Tool, rt.id(q.Exception))
		case err != nil:
			fmt.Fprintf(rt.log, "eval: %s: couldn't withdraw %s (%s): %v: refuse it in KIFF\n", name, q.Tool, rt.id(q.Exception), rt.say(err))
		case w.Withdrawn:
			fmt.Fprintf(rt.log, "eval: %s: withdrew KIFF's hold on %s (%s)\n", name, q.Tool, rt.id(q.Exception))
		default:
			fmt.Fprintf(rt.log, "eval: %s: KIFF's hold on %s (%s) was already %s\n", name, q.Tool, rt.id(q.Exception), w.Status)
		}
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, env func(string) string) (err error) {
	if len(args) > 0 && args[0] == "seed" {
		return runSeed(ctx, args[1:], stdout, stderr, env)
	}
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
	defsDir := fs.String("definitions", "", "-run: the directory of the agents' definitions, <job>.json each, as kiff/tafs builds them (infra/eval-definitions.sh); instead of -model, -price, -jobs and -skill")
	notesDir := fs.String("notes", "", "-run: the directory of the owner's notes, <case>.md each, put in the agent's memory first")
	gateway := fs.String("gateway", kiff.URL, "-run: KIFF's gateway")
	kiffAPI := fs.String("kiff-api", kiff.APIURL, "-run: KIFF's API, where the calls a run left held are withdrawn")
	repeat := fs.Int("repeat", 1, "-run: how many times to run each case; the baseline holds against its mean score")
	seedEach := fs.Bool("seed", false, "-run: seed the case in Stripe test mode (STRIPE_API_KEY) before each of its runs, instead of -ids")
	reportPath := fs.String("report", "", "write a Markdown report here: the configuration, each case over its runs, and each run's checks")
	maxCost := fs.Float64("max-cost", 2, "-run: the most one case's run may cost, in US dollars")
	skillsDir := fs.String("skills", "", "-run: a directory of skills, <name>/<version>/SKILL.md each, that the definitions may name (#101)")
	public := fs.Bool("public", false, "the log is public (CI): print each run's score, cost and failed checks' names only, with no ids or reasons, and no results on stdout; -out and -report hold the rest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *public {
		stdout = io.Discard
		defer func() {
			if err != nil {
				err = errors.New(redactIDs(err.Error()))
			}
		}()
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
		case *repeat < 1:
			return errors.New("-repeat is at least 1")
		case *seedEach && *idsPath != "":
			return errors.New("-seed makes the ids: give -seed or -ids")
		case *seedEach && env("STRIPE_API_KEY") == "":
			return errors.New("set STRIPE_API_KEY to a test-mode key for -seed")
		}
		for _, arg := range fs.Args() { // before anything runs
			switch _, known := scenarios[arg]; {
			case !known:
				return fmt.Errorf("%q: want a case, refunds or chargebacks", arg)
			case *seedEach && arg != "chargebacks":
				return fmt.Errorf("-seed: %s has no seeder yet: give -ids", arg)
			}
		}
		base := registry.Definition{Tenant: evalTenant, Gateway: *gateway, KeyRef: "KIFF_AGENT_KEY", MaxCost: int64(*maxCost * 1_000_000)}
		if *defsDir != "" {
			if *modelID != "" || *price != "" || *jobs != "" || *skillPath != "" {
				return errors.New("-definitions replaces -model, -price, -jobs and -skill: give one or the other")
			}
			rt = newRuntime(newModel(key), kiffKey, base, "", "", stderr)
			rt.defs = *defsDir
			rt.source = *defsDir
			var src struct{ Repo, Commit string }
			if readJSON(filepath.Join(*defsDir, "source.json"), &src) == nil && src.Repo != "" && len(src.Commit) >= 12 {
				rt.source = src.Repo + "@" + src.Commit[:12]
			}
		} else {
			switch {
			case *modelID == "":
				return errors.New("-run needs -model, or -definitions")
			case *jobs == "":
				return errors.New("-run needs -jobs, the directory of the jobs' instructions, or -definitions")
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
			base.Model, base.Price, base.MaxTokens, base.History = *modelID, p, 8192, 20
			rt = newRuntime(newModel(key), kiffKey, base, *jobs, skill, stderr)
			rt.source = "flags"
		}
		if *seedEach {
			rt.notesFor = map[string]string{"chargebacks": seed.ChargebacksNote}
		}
		rt.notes = *notesDir
		if *skillsDir != "" {
			if err := rt.loadSkills(ctx, *skillsDir); err != nil {
				return err
			}
		}
		rt.public = *public
		if u, err := url.Parse(*kiffAPI); err != nil || u.Scheme != "https" || u.Host == "" {
			return errors.New("-kiff-api is an https URL: the agent's key goes there")
		}
		rt.kiffAPI = *kiffAPI
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
		if rt != nil {
			s := scenarios[arg] // checked above
			// Before the first seed: a night the Cards can't fund isn't worth one.
			if err := rt.checkRoom(ctx, s, *repeat); err != nil {
				return err
			}
			for rep := range *repeat {
				if rep > 0 && ctx.Err() != nil {
					skipped = append(skipped, fmt.Sprintf("%s (runs %d to %d)", arg, rep+1, *repeat))
					break
				}
				s := s.WithIDs(ids)
				if *seedEach {
					seeded, _, err := seedChargebacks(ctx, env("STRIPE_API_KEY"))
					if err != nil {
						return fmt.Errorf("%s: seed: %w", s.Name, err)
					}
					s = s.WithIDs(seeded)
				}
				r, err := rt.Run(ctx, s)
				if err != nil {
					return err
				}
				res, err := s.Case().Grade(ctx, r, judge)
				if err != nil {
					return err
				}
				results = append(results, res)
				printResult(stderr, res, *public)
			}
			continue
		}
		name, id, ok := strings.Cut(arg, "=")
		s, known := scenarios[name]
		if !ok || !known || id == "" {
			return fmt.Errorf("%q: want case=session, with a case of refunds or chargebacks", arg)
		}
		r, err := src.Read(ctx, id)
		if err != nil {
			return err
		}
		res, err := s.WithIDs(ids).Case().Grade(ctx, r, judge)
		if err != nil {
			return err
		}
		results = append(results, res)
		printResult(stderr, res, *public)
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
	var lower []string
	if *basePath != "" {
		var base []eval.Result
		if err := readJSON(*basePath, &base); err != nil {
			return err
		}
		lower = eval.Lower(eval.Means(base), eval.Means(results)) // a baseline from -repeat holds its mean too
	}
	if *reportPath != "" {
		var cfgs []eval.Config
		if rt != nil {
			cfgs = rt.configs
		}
		var b bytes.Buffer
		if err := eval.WriteReport(&b, cfgs, results, lower); err != nil {
			return err
		}
		if err := os.WriteFile(*reportPath, b.Bytes(), 0o644); err != nil {
			return err
		}
	}
	if len(lower) > 0 {
		return fmt.Errorf("lower than the baseline: %s", strings.Join(lower, "; "))
	}
	return nil
}

// printResult says how one run graded. A public log (-public) shows no
// session and no check's reason: the reason is the judge's prose about the
// run, which can quote its ids and amounts.
func printResult(stderr io.Writer, res eval.Result, public bool) {
	session := res.Session
	if public {
		session = "…"
	}
	fmt.Fprintf(stderr, "%s  %d/%d  $%.4f a run, $%.4f an item, judge $%.4f  (%s)\n",
		res.Case, res.Passed, res.Graded, dollars(res.CostMicroUSD), dollars(res.PerItemMicro), dollars(res.JudgeMicroUSD), session)
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
		if c.Why != "" && !public {
			fmt.Fprintf(stderr, ": %s", c.Why)
		}
		fmt.Fprintln(stderr)
	}
}

func runSeed(ctx context.Context, args []string, stdout, stderr io.Writer, env func(string) string) error {
	fs := flag.NewFlagSet("eval seed", flag.ContinueOnError)
	fs.SetOutput(stderr)
	outPath := fs.String("out", "", "also write the ids here (JSON)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || fs.Arg(0) != "chargebacks" {
		return errors.New("eval seed: name one case to seed: chargebacks")
	}
	ids, closed, err := seedChargebacks(ctx, env("STRIPE_API_KEY"))
	if err != nil {
		return err
	}
	out, err := json.MarshalIndent(ids, "", "  ")
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
	fmt.Fprintf(stderr, "seeded %d disputes for chargebacks (closed %d left open)\nthe taf's owner note: %s\n", len(ids), closed, seed.ChargebacksNote)
	return nil
}

// seedChargebacks closes what an earlier seed left open, seeds the
// Chargebacks case again, and returns its Stripe ids by reference and how
// many it closed.
func seedChargebacks(ctx context.Context, key string) (map[string][]string, int, error) {
	c, err := seed.New(stripeURL, key, nil)
	if err != nil {
		return nil, 0, err
	}
	closed, err := c.Close(ctx, "chargebacks")
	if err != nil {
		return nil, closed, err
	}
	ids, err := c.Disputes(ctx, "chargebacks", seed.Chargebacks)
	return ids, closed, err
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
