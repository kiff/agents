package main

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/store/memory"
)

// Off by default: the Claude API alone, the model as it was. Bedrock only as
// configured, and never past a day's cap (none set: it refuses).
func TestTheBedrockBackupIsOffUnlessConfigured(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	sc, err := sourceConfigFrom(env(nil))
	if err != nil || sc != (sourceConfig{Source: model.SourceAnthropic}) {
		t.Fatalf("default %+v, %v", sc, err)
	}
	primary := &keyedModel{}
	if got := sourced(primary, sc, aws.Config{Region: "eu-west-1"}, memory.NewUsage()); got != model.Model(primary) {
		t.Fatalf("nothing configured wrapped the model: %T", got)
	}
	sc, err = sourceConfigFrom(env(map[string]string{"AGENTS_BEDROCK_FALLBACK": "on", "AGENTS_BEDROCK_DAILY_USD": "2.50"}))
	if err != nil || !sc.Fallback || sc.Source != model.SourceAnthropic || sc.CapMicro != 2_500_000 {
		t.Fatalf("fallback %+v, %v", sc, err)
	}
	s, ok := sourced(primary, sc, aws.Config{Region: "eu-west-1"}, memory.NewUsage()).(*model.Sourced)
	if !ok || !s.Fallback || s.Cap == nil || s.Cap.MicroUSD != 2_500_000 || s.Exempt || s.FallsBack == nil {
		t.Fatalf("sourced %+v", s)
	}
	// Bedrock as the source, without a cap: wired, and every turn refused.
	sc, _ = sourceConfigFrom(env(map[string]string{"AGENTS_MODEL_SOURCE": "bedrock"}))
	if s, ok := sourced(primary, sc, aws.Config{Region: "eu-west-1"}, memory.NewUsage()).(*model.Sourced); !ok || s.Source != model.SourceBedrock || s.Cap != nil {
		t.Fatalf("source bedrock, no cap: %+v", s)
	}
	for _, bad := range []map[string]string{
		{"AGENTS_MODEL_SOURCE": "openrouter"},
		{"AGENTS_BEDROCK_FALLBACK": "yes"},
		{"AGENTS_BEDROCK_DAILY_USD": "ten"},
		{"AGENTS_BEDROCK_DAILY_USD": "-5"},
		{"AGENTS_BEDROCK_DAILY_USD": "0"},
	} {
		if _, err := sourceConfigFrom(env(bad)); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

// The backup's day total is kept in the usage table under a key no tenant
// has: a turn reserves within the cap, then settles to what it cost.
func TestTheBackupsDayTotalIsKept(t *testing.T) {
	use := memory.NewUsage()
	d := daySpend{use}
	if ok, err := d.Reserve(ctx, "2026-11-07", 1_200_000, 1_500_000); err != nil || !ok {
		t.Fatalf("a reservation within the cap: %v, %v", ok, err)
	}
	if ok, _ := d.Reserve(ctx, "2026-11-07", 400_000, 1_500_000); ok {
		t.Fatal("a reservation past the cap was taken")
	}
	if err := d.Add(ctx, "2026-11-07", -900_000); err != nil { // the turn cost 300,000
		t.Fatal(err)
	}
	if got, err := use.Get(ctx, backupTenant, "2026-11-07"); err != nil || got.Spent != 300_000 {
		t.Fatalf("spent %d, %v", got.Spent, err)
	}
	if got, _ := use.Get(ctx, backupTenant, "2026-11-08"); got.Spent != 0 {
		t.Fatalf("the next day starts at %d", got.Spent)
	}
	if _, err := d.Reserve(ctx, "07/11/2026", 1, 2); err == nil {
		t.Fatal("a day that isn't one was taken")
	}
}
