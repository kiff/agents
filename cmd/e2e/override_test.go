package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/registry"
)

// infra/eval-override.sh sets Coach's and every taf's definition to the
// Eval's model and effort, with that model's prices so the cost caps count
// what it charges; empty inputs change nothing, and a model it can't price,
// or an effort that isn't one, is refused before anything is written.
func TestEvalOverrideSetsModelEffortAndPrices(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq isn't installed")
	}
	opus := `{"Input":4000000,"Output":20000000,"CacheRead":200000,"CacheWrite":5000000,"CacheWrite1h":8000000}`
	defs := func() string {
		dir := t.TempDir()
		for name, body := range map[string]string{
			"setup.json":       `{"model":"claude-opus-5-5","effort":"high","speed":"standard","price":` + opus + `,"price_fast":` + opus + `,"max_cost_micro_usd":6000000}`,
			"chargebacks.json": `{"model":"claude-opus-5-5","effort":"medium","price":` + opus + `,"max_cost_micro_usd":2000000}`,
			"source.json":      `{"repo":"kiff/tafs","commit":"abc"}`,
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	read := func(dir, name string) map[string]json.RawMessage {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	run := func(dir, m, effort string) error {
		_, err := exec.Command("bash", "../../infra/eval-override.sh", dir, m, effort).CombinedOutput()
		return err
	}

	dir := defs()
	if err := run(dir, "claude-sonnet-5-5", "low"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"setup.json", "chargebacks.json"} {
		d := read(dir, name)
		var price model.Price
		if err := json.Unmarshal(d["price"], &price); err != nil {
			t.Fatal(err)
		}
		if string(d["model"]) != `"claude-sonnet-5-5"` || string(d["effort"]) != `"low"` || price.Input != 2_000_000 || price.Output != 10_000_000 || !price.Caches() || !price.CachesAnHour() {
			t.Fatalf("%s: %s %s %+v", name, d["model"], d["effort"], price)
		}
		if string(d["max_cost_micro_usd"]) == "" {
			t.Fatalf("%s lost its cost cap", name)
		}
	}
	if _, ok := read(dir, "chargebacks.json")["price_fast"]; ok {
		t.Fatal("a taf gained a fast price")
	}
	var o struct{ Model, Effort string }
	if err := json.Unmarshal(read(dir, "source.json")["override"], &o); err != nil || o.Model != "claude-sonnet-5-5" || o.Effort != "low" {
		t.Fatalf("source.json override: %v %+v", err, o)
	}
	// The definitions still load as kiff/agents' registry reads them.
	var d registry.Definition
	b, _ := os.ReadFile(filepath.Join(dir, "setup.json"))
	if err := json.Unmarshal(b, &d); err != nil || d.Model != "claude-sonnet-5-5" || d.Effort != "low" {
		t.Fatalf("setup.json as a definition: %v %+v", err, d)
	}

	// Effort alone keeps the model and its prices.
	dir = defs()
	if err := run(dir, "", "low"); err != nil {
		t.Fatal(err)
	}
	var kept, want model.Price
	d = registry.Definition{}
	if b, _ := os.ReadFile(filepath.Join(dir, "setup.json")); json.Unmarshal(b, &d) != nil {
		t.Fatal("setup.json doesn't read")
	}
	_ = json.Unmarshal(read(dir, "setup.json")["price"], &kept)
	_ = json.Unmarshal([]byte(opus), &want)
	if d.Model != "claude-opus-5-5" || d.Effort != "low" || kept != want {
		t.Fatalf("effort alone: %s %s %+v", d.Model, d.Effort, kept)
	}

	// Nothing set changes nothing; refusals write nothing.
	for _, c := range []struct{ model, effort string }{{"", ""}, {"gpt-9", "low"}, {"claude-sonnet-5-5", "turbo"}} {
		dir = defs()
		before := read(dir, "setup.json")
		err := run(dir, c.model, c.effort)
		if (c.model != "" || c.effort != "") && err == nil {
			t.Fatalf("%q %q wasn't refused", c.model, c.effort)
		}
		if after := read(dir, "setup.json"); string(after["model"]) != string(before["model"]) || string(after["effort"]) != string(before["effort"]) {
			t.Fatalf("%q %q changed setup.json", c.model, c.effort)
		}
	}
}
