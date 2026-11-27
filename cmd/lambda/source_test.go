package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/store/memory"
)

// Off by default: the Claude API alone, the model as it was. Bedrock only as
// configured, and never past a day's cap (none set: it refuses).
func TestTheBedrockBackupIsOffUnlessConfigured(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	sc, err := sourceConfigFrom(env(nil))
	if err != nil || sc.Source != model.SourceAnthropic || sc.Fallback || sc.CapMicro != 0 || sc.AccountMicro != 0 || sc.ReservedTo != nil || sc.ReserveMicro != 0 {
		t.Fatalf("default %+v, %v", sc, err)
	}
	primary := &keyedModel{}
	if got := sourced(primary, sc, aws.Config{Region: "eu-west-1"}, memory.NewUsage(), nil); got != model.Model(primary) {
		t.Fatalf("nothing configured wrapped the model: %T", got)
	}
	sc, err = sourceConfigFrom(env(map[string]string{"AGENTS_BEDROCK_FALLBACK": "on", "AGENTS_BEDROCK_DAILY_USD": "2.50"}))
	if err != nil || !sc.Fallback || sc.Source != model.SourceAnthropic || sc.CapMicro != 2_500_000 {
		t.Fatalf("fallback %+v, %v", sc, err)
	}
	s, ok := sourced(primary, sc, aws.Config{Region: "eu-west-1"}, memory.NewUsage(), nil).(*model.Sourced)
	if !ok || !s.Fallback || s.Cap == nil || s.Cap.MicroUSD != 2_500_000 || s.Exempt || s.FallsBack == nil {
		t.Fatalf("sourced %+v", s)
	}
	// Bedrock as the source, without a cap: wired, and every turn refused.
	sc, _ = sourceConfigFrom(env(map[string]string{"AGENTS_MODEL_SOURCE": "bedrock"}))
	if s, ok := sourced(primary, sc, aws.Config{Region: "eu-west-1"}, memory.NewUsage(), nil).(*model.Sourced); !ok || s.Source != model.SourceBedrock || s.Cap != nil {
		t.Fatalf("source bedrock, no cap: %+v", s)
	}
	for _, bad := range []map[string]string{
		{"AGENTS_MODEL_SOURCE": "openrouter"},
		{"AGENTS_BEDROCK_FALLBACK": "yes"},
		{"AGENTS_BEDROCK_DAILY_USD": "ten"},
		{"AGENTS_BEDROCK_DAILY_USD": "-5"},
		{"AGENTS_BEDROCK_DAILY_USD": "0"},
		{"AGENTS_BEDROCK_DAILY_USD": "60", "AGENTS_BEDROCK_ACCOUNT_DAILY_USD": "six"},
		{"AGENTS_BEDROCK_DAILY_USD": "60", "AGENTS_BEDROCK_ACCOUNT_DAILY_USD": "61"},
		{"AGENTS_BEDROCK_DAILY_USD": "60", "AGENTS_BEDROCK_RESERVED_TENANTS": "t-review", "AGENTS_BEDROCK_RESERVED_USD": "60"},
		{"AGENTS_BEDROCK_DAILY_USD": "60", "AGENTS_BEDROCK_RESERVED_TENANTS": "t-review", "AGENTS_BEDROCK_RESERVED_USD": "0"},
		{"AGENTS_BEDROCK_DAILY_USD": "60", "AGENTS_BEDROCK_TRIAL_DAILY_USD": "three"},
		{"AGENTS_BEDROCK_DAILY_USD": "60", "AGENTS_BEDROCK_TRIAL_DAILY_USD": "61"},
		{"AGENTS_BEDROCK_DAILY_USD": "60", "AGENTS_BEDROCK_UNBOUNDED_DAILY_USD": "0"},
		{"AGENTS_BEDROCK_DAILY_USD": "60", "AGENTS_BEDROCK_RESERVED_TENANTS": "t-review", "AGENTS_BEDROCK_RESERVED_USD": "12",
			"AGENTS_BEDROCK_UNBOUNDED_DAILY_USD": "49"},
		{"AGENTS_BEDROCK_DAILY_USD": "60", "AGENTS_BEDROCK_TENANT_CAPS": "t-pilot"},
		{"AGENTS_BEDROCK_DAILY_USD": "60", "AGENTS_BEDROCK_TENANT_CAPS": "t-pilot=fifteen"},
		{"AGENTS_BEDROCK_DAILY_USD": "60", "AGENTS_BEDROCK_TENANT_CAPS": "t-pilot=0"},
		{"AGENTS_BEDROCK_DAILY_USD": "60", "AGENTS_BEDROCK_TENANT_CAPS": "=15"},
		{"AGENTS_BEDROCK_DAILY_USD": "60", "AGENTS_BEDROCK_TENANT_CAPS": "t-pilot=15,t-pilot=10"},
		// ParseFloat takes these with no error; each would fail open.
		{"AGENTS_BEDROCK_DAILY_USD": "60", "AGENTS_BEDROCK_TENANT_CAPS": "t-pilot=NaN"},
		{"AGENTS_BEDROCK_DAILY_USD": "60", "AGENTS_BEDROCK_TENANT_CAPS": "t-pilot=nan"},
		{"AGENTS_BEDROCK_DAILY_USD": "60", "AGENTS_BEDROCK_TENANT_CAPS": "t-pilot=Inf"},
		{"AGENTS_BEDROCK_DAILY_USD": "NaN"},
		{"AGENTS_BEDROCK_DAILY_USD": "+Inf"},
		{"AGENTS_BEDROCK_DAILY_USD": "60", "AGENTS_BEDROCK_ACCOUNT_DAILY_USD": "NaN"},
		{"AGENTS_BEDROCK_DAILY_USD": "60", "AGENTS_BEDROCK_TRIAL_DAILY_USD": "NaN"},
		{"AGENTS_BEDROCK_DAILY_USD": "60", "AGENTS_BEDROCK_UNBOUNDED_DAILY_USD": "Infinity"},
		{"AGENTS_BEDROCK_DAILY_USD": "60", "AGENTS_BEDROCK_RESERVED_TENANTS": "t-review", "AGENTS_BEDROCK_RESERVED_USD": "12",
			"AGENTS_BEDROCK_TENANT_CAPS": "t-pilot=49"},
	} {
		if _, err := sourceConfigFrom(env(bad)); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

// Each account's share and the reviewers' slice are parts of the day's cap:
// by default a tenth of it each, and a fifth for the reviewers once any are
// listed; set, they're taken as set.
func TestTheSharesOfTheBackupsDay(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	sc, err := sourceConfigFrom(env(map[string]string{"AGENTS_MODEL_SOURCE": "bedrock", "AGENTS_BEDROCK_DAILY_USD": "60"}))
	if err != nil || sc.AccountMicro != 6_000_000 || sc.ReservedTo != nil || sc.ReserveMicro != 0 {
		t.Fatalf("defaults %+v, %v", sc, err)
	}
	sc, err = sourceConfigFrom(env(map[string]string{"AGENTS_MODEL_SOURCE": "bedrock", "AGENTS_BEDROCK_DAILY_USD": "60", "AGENTS_BEDROCK_RESERVED_TENANTS": " t-review, t-staff ,"}))
	if err != nil || len(sc.ReservedTo) != 2 || !sc.ReservedTo["t-review"] || !sc.ReservedTo["t-staff"] || sc.ReserveMicro != 12_000_000 {
		t.Fatalf("reviewers listed %+v, %v", sc, err)
	}
	sc, err = sourceConfigFrom(env(map[string]string{"AGENTS_MODEL_SOURCE": "bedrock", "AGENTS_BEDROCK_DAILY_USD": "60",
		"AGENTS_BEDROCK_ACCOUNT_DAILY_USD": "4.5", "AGENTS_BEDROCK_RESERVED_TENANTS": "t-review", "AGENTS_BEDROCK_RESERVED_USD": "9"}))
	if err != nil || sc.AccountMicro != 4_500_000 || sc.ReserveMicro != 9_000_000 {
		t.Fatalf("set %+v, %v", sc, err)
	}
	s := sourced(&keyedModel{}, sc, aws.Config{Region: "eu-west-1"}, memory.NewUsage(), nil).(*model.Sourced)
	if c := s.Cap; c.MicroUSD != 60_000_000 || c.AccountMicroUSD != 4_500_000 || c.ReservedMicroUSD != 9_000_000 || !c.Reserved["t-review"] {
		t.Fatalf("the cap %+v", c)
	}
}

// The trial's share is $3 and an unbounded account's two fifths of the cap,
// by default; set, they're taken as set. A cap too small for the trial's $3
// takes its default down to what the others may take of the day.
func TestTheTrialsAndAnUnboundedAccountsShares(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	sc, err := sourceConfigFrom(env(map[string]string{"AGENTS_MODEL_SOURCE": "bedrock", "AGENTS_BEDROCK_DAILY_USD": "500",
		"AGENTS_BEDROCK_ACCOUNT_DAILY_USD": "50", "AGENTS_BEDROCK_RESERVED_TENANTS": "t-review"}))
	if err != nil || sc.TrialMicro != 3_000_000 || sc.UnboundedMicro != 200_000_000 {
		t.Fatalf("defaults %+v, %v", sc, err)
	}
	s := sourced(&keyedModel{}, sc, aws.Config{Region: "eu-west-1"}, memory.NewUsage(), nil).(*model.Sourced)
	if c := s.Cap; c.TrialMicroUSD != 3_000_000 || c.MaxMicroUSD != 200_000_000 {
		t.Fatalf("the cap %+v", c)
	}
	sc, err = sourceConfigFrom(env(map[string]string{"AGENTS_MODEL_SOURCE": "bedrock", "AGENTS_BEDROCK_DAILY_USD": "500",
		"AGENTS_BEDROCK_TRIAL_DAILY_USD": "2.5", "AGENTS_BEDROCK_UNBOUNDED_DAILY_USD": "300"}))
	if err != nil || sc.TrialMicro != 2_500_000 || sc.UnboundedMicro != 300_000_000 {
		t.Fatalf("set %+v, %v", sc, err)
	}
	sc, err = sourceConfigFrom(env(map[string]string{"AGENTS_BEDROCK_FALLBACK": "on", "AGENTS_BEDROCK_DAILY_USD": "2.50"}))
	if err != nil || sc.TrialMicro != 2_500_000 {
		t.Fatalf("a small cap %+v, %v", sc, err)
	}
}

// The backup's day total is kept in the usage table under a key no tenant
// has: a turn reserves within the cap, then settles to what it cost. The
// account's part is kept in its own usage, so erasing it erases its part.
func TestTheBackupsDayTotalIsKept(t *testing.T) {
	use := memory.NewUsage()
	d := daySpend{use: use}
	if full, err := d.Reserve(ctx, "2026-11-07", "t1", 1_200_000, 1_500_000, 0); err != nil || full != model.NotFull {
		t.Fatalf("a reservation within the cap: %v, %v", full, err)
	}
	if full, _ := d.Reserve(ctx, "2026-11-07", "t1", 400_000, 1_500_000, 0); full != model.DayFull {
		t.Fatalf("a reservation past the cap: %v", full)
	}
	if err := d.Add(ctx, "2026-11-07", "t1", -900_000); err != nil { // the turn cost 300,000
		t.Fatal(err)
	}
	if got, err := use.Get(ctx, backupTenant, "2026-11-07"); err != nil || got.Spent != 300_000 {
		t.Fatalf("spent %d, %v", got.Spent, err)
	}
	if got, _ := use.Get(ctx, backupTenant, "2026-11-08"); got.Spent != 0 {
		t.Fatalf("the next day starts at %d", got.Spent)
	}
	if got, _ := use.Get(ctx, "t1", "bedrock|2026-11-07"); got.Spent != 300_000 {
		t.Fatalf("the account's part %d", got.Spent)
	}
	if err := use.EraseTenant(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := use.Get(ctx, "t1", "bedrock|2026-11-07"); got.Spent != 0 {
		t.Fatalf("the erased account's part %d", got.Spent)
	}
	if _, err := d.Reserve(ctx, "07/11/2026", "t1", 1, 2, 0); err == nil {
		t.Fatal("a day that isn't one was taken")
	}
}

// An account past its share is refused without touching the day, and said;
// a turn the day refuses after its share was reserved gives the share back,
// as does one whose day reservation fails: both, or neither.
func TestAnAccountsShareIsReservedWithTheDayOrNotAtAll(t *testing.T) {
	use := &failingDay{Usage: memory.NewUsage()}
	var said []string
	d := daySpend{use: use, log: func(msg string, _ error) { said = append(said, msg) }}
	share := func(tenant string) int64 {
		t.Helper()
		p, err := use.Get(ctx, tenant, "bedrock|2026-11-25")
		if err != nil {
			t.Fatal(err)
		}
		return p.Spent
	}
	if full, err := d.Reserve(ctx, "2026-11-25", "t-loop", 600_000, 10_000_000, 1_000_000); err != nil || full != model.NotFull {
		t.Fatalf("within its share: %v, %v", full, err)
	}
	if full, err := d.Reserve(ctx, "2026-11-25", "t-loop", 600_000, 10_000_000, 1_000_000); err != nil || full != model.ShareFull {
		t.Fatalf("past its share: %v, %v", full, err)
	}
	if day, _ := use.Get(ctx, backupTenant, "2026-11-25"); day.Spent != 600_000 || share("t-loop") != 600_000 || len(said) != 1 || said[0] != ShareReached {
		t.Fatalf("day %d, share %d, said %v", day.Spent, share("t-loop"), said)
	}
	// The day is full for this turn: its share is given back.
	if full, err := d.Reserve(ctx, "2026-11-25", "t-other", 500_000, 1_000_000, 1_000_000); err != nil || full != model.DayFull || share("t-other") != 0 {
		t.Fatalf("the day full: %v, %v, share %d", full, err, share("t-other"))
	}
	// The day's reservation fails: the share is given back, and the error said.
	use.fail = errors.New("throttled")
	if _, err := d.Reserve(ctx, "2026-11-25", "t-other", 100_000, 10_000_000, 1_000_000); !errors.Is(err, use.fail) || share("t-other") != 0 {
		t.Fatalf("the day's reservation failed: %v, share %d", err, share("t-other"))
	}
}

// failingDay is a usage store whose day reservations fail with fail.
type failingDay struct {
	*memory.Usage
	fail error
}

func (f *failingDay) Reserve(ctx context.Context, tenant, period string, micro, limit int64, until time.Time) (bool, error) {
	if tenant == backupTenant && f.fail != nil {
		return false, f.fail
	}
	return f.Usage.Reserve(ctx, tenant, period, micro, limit, until)
}

// The day says when it passes 50% and 80% of its cap, each once, by the turn
// that crossed it, and says each turn the cap refuses: the lines an alarm
// tells the owner.
func TestTheBackupsDaySaysWhenItNearsAndReachesItsCap(t *testing.T) {
	var said []string
	d := daySpend{use: memory.NewUsage(), log: func(msg string, _ error) { said = append(said, msg) }}
	reserve := func(micro int64) bool {
		t.Helper()
		full, err := d.Reserve(ctx, "2026-11-25", "t1", micro, 1_000_000, 0)
		if err != nil {
			t.Fatal(err)
		}
		return full == model.NotFull
	}
	reserve(400_000) // 40%: nothing
	if len(said) != 0 {
		t.Fatalf("said %v below 50%%", said)
	}
	reserve(200_000) // 60%: half crossed
	reserve(100_000) // 70%: already said
	if len(said) != 1 || said[0] != CapHalf {
		t.Fatalf("said %v", said)
	}
	reserve(150_000) // 85%: crossed
	reserve(50_000)  // 90%: already said
	if len(said) != 2 || said[1] != CapNear {
		t.Fatalf("said %v", said)
	}
	if reserve(200_000) { // past the cap: refused, and said
		t.Fatal("a turn past the cap was taken")
	}
	if len(said) != 3 || said[2] != CapReached {
		t.Fatalf("said %v", said)
	}
	// One turn across both lines says both.
	said = nil
	d.Reserve(ctx, "2026-11-26", "t1", 900_000, 1_000_000, 0)
	if len(said) != 2 || said[0] != CapHalf || said[1] != CapNear {
		t.Fatalf("one turn across both: said %v", said)
	}
}

// The AI consent's read says where new runs' turns go, as configured:
// Bedrock in the EU; the Claude API alone; or the Claude API with Bedrock as
// a fallback, so either may get the words.
func TestWhereTurnsRunAsConfigured(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	for _, c := range []struct {
		env  map[string]string
		want server.AISource
	}{
		{nil, server.AISource{Source: "anthropic"}},
		{map[string]string{"AGENTS_MODEL_SOURCE": "bedrock", "AGENTS_BEDROCK_DAILY_USD": "60"}, server.AISource{Source: "bedrock", Geo: "eu"}},
		{map[string]string{"AGENTS_MODEL_SOURCE": "bedrock", "AGENTS_BEDROCK_FALLBACK": "on"}, server.AISource{Source: "bedrock", Geo: "eu"}},
		{map[string]string{"AGENTS_BEDROCK_FALLBACK": "on", "AGENTS_BEDROCK_DAILY_USD": "60"}, server.AISource{Source: "anthropic", Fallback: "bedrock"}},
	} {
		sc, err := sourceConfigFrom(env(c.env))
		if err != nil {
			t.Fatal(err)
		}
		if got := aiSource(sc); got != c.want {
			t.Errorf("%v: %+v, want %+v", c.env, got, c.want)
		}
	}
}

// Some accounts' own shares: tenant=usd pairs, taken as set and handed to
// the cap; none by default.
func TestSomeAccountsOwnShares(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	sc, err := sourceConfigFrom(env(map[string]string{"AGENTS_MODEL_SOURCE": "bedrock", "AGENTS_BEDROCK_DAILY_USD": "500"}))
	if err != nil || sc.TenantMicro != nil {
		t.Fatalf("default %+v, %v", sc, err)
	}
	sc, err = sourceConfigFrom(env(map[string]string{"AGENTS_MODEL_SOURCE": "bedrock", "AGENTS_BEDROCK_DAILY_USD": "500",
		"AGENTS_BEDROCK_TENANT_CAPS": " t-pilot=15 , t-demo=2.5,"}))
	if err != nil || len(sc.TenantMicro) != 2 || sc.TenantMicro["t-pilot"] != 15_000_000 || sc.TenantMicro["t-demo"] != 2_500_000 {
		t.Fatalf("set %+v, %v", sc, err)
	}
	s := sourced(&keyedModel{}, sc, aws.Config{Region: "eu-west-1"}, memory.NewUsage(), nil).(*model.Sourced)
	if got := s.Cap.Share("t-pilot", model.Plan{Kind: model.PlanUnbounded}); got != 15_000_000 {
		t.Fatalf("the pilot's share on the cap: %d", got)
	}
}
