// Command eval grades recorded runs against the eval cases and prints a score
// and cost per case.
//
//	eval [-judge-model id -judge-price in,out] [-ids seed.json] [-baseline base.json] [-out results.json] case=session...
//
// Each argument names a case (refunds, chargebacks) and the Managed Agents
// session of the run to grade, read with ANTHROPIC_API_KEY from the
// environment. With -baseline, a case that scores lower than it did there
// fails the command. -ids is the seed's Stripe ids by item reference.
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
	"strconv"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/kiff/agents/eval"
	"github.com/kiff/agents/eval/cases"
	"github.com/kiff/agents/eval/managed"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/model/anthropic"
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

func run(ctx context.Context, args []string, stdout, stderr io.Writer, env func(string) string) error {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	idsPath := fs.String("ids", "", "the seed's Stripe ids by item reference (JSON)")
	basePath := fs.String("baseline", "", "results to compare against (JSON); a lower score fails")
	outPath := fs.String("out", "", "also write the results here (JSON)")
	judgeModel := fs.String("judge-model", "", "the judge's model id; without it, judged checks are skipped")
	judgePrice := fs.String("judge-price", "", "the judge's input,output price in millionths of a dollar per million tokens")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
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
	var judge *eval.Judge
	if *judgeModel != "" {
		p, err := parsePrice(*judgePrice)
		if err != nil {
			return err
		}
		judge = &eval.Judge{Model: newModel(key), ModelID: *judgeModel, Price: p}
	}

	var results []eval.Result
	for _, arg := range fs.Args() {
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
		fmt.Fprintf(stderr, "%s  %d/%d  $%.4f a run, $%.4f an item, judge $%.4f  (%s)\n",
			res.Case, res.Passed, res.Graded, dollars(res.CostMicroUSD), dollars(res.PerItemMicro), dollars(res.JudgeMicroUSD), res.Session)
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
	in, out, ok := strings.Cut(s, ",")
	i, err1 := strconv.ParseInt(strings.TrimSpace(in), 10, 64)
	o, err2 := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if !ok || err1 != nil || err2 != nil || i <= 0 || o <= 0 {
		return model.Price{}, errors.New("-judge-price is input,output in millionths of a dollar per million tokens, both positive")
	}
	return model.Price{Input: i, Output: o}, nil
}

func dollars(micro int64) float64 { return float64(micro) / 1e6 }
