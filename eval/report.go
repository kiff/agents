package eval

import (
	"fmt"
	"io"
	"slices"
	"strings"
)

// Summary is a case over its runs: the mean score and its range, the mean
// cost per run and per item, and how often each check failed. One run says
// little: the same case scores differently run to run, so a baseline is
// held against the mean.
type Summary struct {
	Case         string  `json:"case"`
	Runs         int     `json:"runs"`
	Score        float64 `json:"score"`
	Min          float64 `json:"min"`
	Max          float64 `json:"max"`
	CostMicroUSD int64   `json:"cost_micro_usd"`
	PerItemMicro int64   `json:"cost_per_item_micro_usd"`
	Turns        float64 `json:"turns"`
	CacheShare   float64 `json:"cache_share"`
	// Failed counts, by check, the runs it failed in.
	Failed map[string]int `json:"failed,omitempty"`
}

// Summarize groups results by case, in the order cases first appear.
func Summarize(results []Result) []Summary {
	var out []Summary
	at := map[string]int{}
	for _, r := range results {
		i, ok := at[r.Case]
		if !ok {
			i = len(out)
			at[r.Case] = i
			out = append(out, Summary{Case: r.Case, Min: r.Score, Max: r.Score})
		}
		s := &out[i]
		s.Runs++
		s.Score += r.Score
		s.Min, s.Max = min(s.Min, r.Score), max(s.Max, r.Score)
		s.CostMicroUSD += r.CostMicroUSD
		s.PerItemMicro += r.PerItemMicro
		s.Turns += float64(r.Context.Turns)
		s.CacheShare += r.Context.CacheShare
		for _, c := range r.Checks {
			if !c.Pass && !c.Skipped {
				if s.Failed == nil {
					s.Failed = map[string]int{}
				}
				s.Failed[c.Name]++
			}
		}
	}
	for i := range out {
		s := &out[i]
		n := float64(s.Runs)
		s.Score /= n
		s.CostMicroUSD = (s.CostMicroUSD + int64(s.Runs) - 1) / int64(s.Runs) // rounded up, like every cost here
		s.PerItemMicro = (s.PerItemMicro + int64(s.Runs) - 1) / int64(s.Runs)
		s.Turns /= n
		s.CacheShare /= n
	}
	return out
}

// Means is each case's mean score as a Result, for Lower.
func Means(results []Result) []Result {
	var out []Result
	for _, s := range Summarize(results) {
		out = append(out, Result{Case: s.Case, Score: s.Score})
	}
	return out
}

// Config is what a job's agent ran as, for the report: where its
// definition came from, and what the definition said.
type Config struct {
	Job          string
	Source       string   // e.g. "kiff/tafs@1d1ed42e64e8", or "flags"
	Model        string   //
	Effort       string   // "" is the provider's default
	MaxTokens    int64    //
	Price        string   // as -price takes it
	Instructions string   // its size and digest
	Tools        []string // as KIFF's gateway listed them
	Skills       []string // the skills it names, name@version (#101)
}

// WriteReport writes the run as Markdown: each job's configuration, each
// case's summary, and each run's checks with why they failed.
func WriteReport(w io.Writer, cfgs []Config, results []Result, lower []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Eval report\n\n")
	if len(lower) > 0 {
		fmt.Fprintf(&b, "**Lower than the baseline:** %s\n\n", strings.Join(lower, "; "))
	}
	for _, cfg := range cfgs {
		fmt.Fprintf(&b, "## Configuration: %s\n\n| | |\n|---|---|\n", cfg.Job)
		effort := cfg.Effort
		if effort == "" {
			effort = "the provider's default"
		}
		tools := slices.Clone(cfg.Tools)
		slices.Sort(tools)
		for _, row := range [][2]string{
			{"Definition", cfg.Source}, {"Model", cfg.Model}, {"Effort", effort},
			{"Max tokens", fmt.Sprint(cfg.MaxTokens)}, {"Price", cfg.Price},
			{"Instructions", cfg.Instructions}, {"Tools", strings.Join(tools, ", ")}, {"Skills", strings.Join(cfg.Skills, ", ")},
		} {
			fmt.Fprintf(&b, "| %s | %s |\n", row[0], cell(row[1]))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "## Cases\n\n| Case | Runs | Score | Range | $ / run | $ / item | Turns | From cache | Failed checks |\n|---|---:|---:|---|---:|---:|---:|---:|---|\n")
	for _, s := range Summarize(results) {
		var failed []string
		for name, n := range s.Failed {
			failed = append(failed, fmt.Sprintf("%s (%d)", name, n))
		}
		slices.Sort(failed)
		fmt.Fprintf(&b, "| %s | %d | %.2f | %.2f–%.2f | %.4f | %.4f | %.1f | %.0f%% | %s |\n",
			s.Case, s.Runs, s.Score, s.Min, s.Max, float64(s.CostMicroUSD)/1e6, float64(s.PerItemMicro)/1e6,
			s.Turns, 100*s.CacheShare, cell(strings.Join(failed, ", ")))
	}
	fmt.Fprintf(&b, "\n## Runs\n")
	for i, r := range results {
		fmt.Fprintf(&b, "\n### %s, run %d: %d/%d, $%.4f\n\n", r.Case, i+1, r.Passed, r.Graded, float64(r.CostMicroUSD)/1e6)
		if cx := r.Context; cx.Turns > 0 {
			fmt.Fprintf(&b, "%d turns, %d input tokens, %.0f%% from the cache, peak turn %d tokens.\n\n",
				cx.Turns, cx.PromptTokens, 100*cx.CacheShare, cx.PeakPromptTokens)
		}
		for _, c := range r.Checks {
			mark := "pass"
			switch {
			case c.Skipped:
				mark = "skip"
			case !c.Pass:
				mark = "**FAIL**"
			}
			fmt.Fprintf(&b, "- %s %s", mark, c.Name)
			if c.Why != "" {
				fmt.Fprintf(&b, ": %s", c.Why)
			}
			b.WriteString("\n")
		}
		report := strings.TrimSpace(r.Report)
		if report == "" {
			report = "(none: the agent said nothing last)"
		}
		fmt.Fprintf(&b, "\nIts report, as the checks read it:\n\n> %s\n", strings.ReplaceAll(report, "\n", "\n> "))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// cell keeps text in one table cell.
func cell(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "|", `\|`)
	if s == "" {
		return "–"
	}
	return s
}
