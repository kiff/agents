package eval_test

import (
	"strings"
	"testing"

	"github.com/kiff/agents/eval"
)

func result(c string, score float64, cost int64, failed ...string) eval.Result {
	r := eval.Result{Case: c, Score: score, CostMicroUSD: cost, PerItemMicro: cost / 2, Context: eval.Context{Turns: 4, CacheShare: 0.5}}
	for _, f := range failed {
		r.Checks = append(r.Checks, eval.CheckResult{Name: f, Grade: eval.Grade{Why: "no"}})
	}
	r.Checks = append(r.Checks, eval.CheckResult{Name: "skipped", Grade: eval.Grade{Skipped: true}})
	return r
}

// A case over its runs: the mean score and its range, costs rounded up,
// and how often each check failed; skipped checks never count (#78).
func TestSummarize(t *testing.T) {
	got := eval.Summarize([]eval.Result{
		result("chargebacks", 4.0/6, 300_000, "does the job"),
		result("refunds", 1, 100_000),
		result("chargebacks", 2.0/6, 400_001, "does the job", "stops on held"),
	})
	if len(got) != 2 || got[0].Case != "chargebacks" || got[1].Case != "refunds" {
		t.Fatalf("summaries %+v", got)
	}
	c := got[0]
	if c.Runs != 2 || c.Score != 0.5 || c.Min != 2.0/6 || c.Max != 4.0/6 || c.CostMicroUSD != 350_001 ||
		c.Turns != 4 || c.CacheShare != 0.5 || c.Failed["does the job"] != 2 || c.Failed["stops on held"] != 1 || c.Failed["skipped"] != 0 {
		t.Fatalf("chargebacks %+v", c)
	}
	means := eval.Means([]eval.Result{result("c", 1, 0), result("c", 0, 0)})
	if len(means) != 1 || means[0].Score != 0.5 {
		t.Fatalf("means %+v", means)
	}
	// The baseline holds against the mean, not each run.
	if lower := eval.Lower([]eval.Result{{Case: "c", Score: 0.5}}, means); len(lower) != 0 {
		t.Fatalf("lower %v", lower)
	}
}

func TestWriteReport(t *testing.T) {
	var b strings.Builder
	cfg := eval.Config{Job: "chargebacks", Source: "kiff/tafs@1d1ed42e64e8", Model: "claude-opus-5-5", Effort: "high", MaxTokens: 16000,
		Price: "4000000,20000000,200000,5000000", Instructions: "17353 bytes, sha256 ab12", Tools: []string{"update_dispute", "close_dispute"}}
	r := result("chargebacks", 0.5, 384_200, "stops on held")
	r.Checks[0].Why = "wrote T-2005 again | with update_dispute"
	if err := eval.WriteReport(&b, []eval.Config{cfg}, []eval.Result{r}, []string{"chargebacks: 0.50, was 0.67"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"**Lower than the baseline:** chargebacks: 0.50, was 0.67", "## Configuration: chargebacks",
		"| Definition | kiff/tafs@1d1ed42e64e8 |", "| Effort | high |", "| Tools | close_dispute, update_dispute |",
		"| chargebacks | 1 | 0.50 | 0.50–0.50 | 0.3842 |", "stops on held (1)",
		"- **FAIL** stops on held: wrote T-2005 again | with update_dispute", "- skip skipped",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("no %q in\n%s", want, b.String())
		}
	}
}
