package kiff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/registry"
)

// AllowanceKept is how long an allowed answer is kept per agent, so a run
// doesn't wait on KIFF each time. A refusal isn't kept: a raise or a pack
// lets the next run start at once. For up to this long after the balance
// is used, a run can still start.
const AllowanceKept = time.Minute

// Allowance asks KIFF whether a new run of the key's agent may start: GET
// /v1/me/credits/allowance with the agent's own key. It answers whether,
// and why not: KIFF's code (credits_used_up, limit_reached or no_plan), and
// the account's plan as KIFF says it ("ai"), for its share of Bedrock's day
// (model.DailyCap.Share). Any other answer is an error, a 404 included, and
// the key never appears in one: the route is KIFF's, so a 404 is a KIFF
// fault, never a run let through unchecked. A plan KIFF doesn't say, or
// says in a way not read here, is the zero Plan: the flat share.
func Allowance(ctx context.Context, hc *http.Client, apiURL, key string) (allowed bool, why string, plan model.Plan, err error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(apiURL, "/")+"/v1/me/credits/allowance", nil)
	if err != nil {
		return false, "", plan, fmt.Errorf("kiff: allowance: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := hc.Do(req)
	if err != nil {
		return false, "", plan, fmt.Errorf("kiff: allowance: %w", scrub(err, key))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if resp.StatusCode != http.StatusOK {
		return false, "", plan, fmt.Errorf("kiff: allowance: %s: %s", resp.Status, strings.TrimSpace(scrub(errors.New(string(b)), key).Error()))
	}
	var out struct {
		Allowed *bool  `json:"allowed"`
		Why     string `json:"why"`
		AI      *struct {
			Kind           string `json:"kind"`
			PeriodMicroUSD *int64 `json:"period_micro_usd"`
		} `json:"ai"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Allowed == nil || (!*out.Allowed && out.Why == "") {
		return false, "", plan, errors.New("kiff: allowance: not an answer KIFF gives")
	}
	if a := out.AI; a != nil {
		switch a.Kind {
		case model.PlanNone, model.PlanUnbounded:
			plan.Kind = a.Kind
		case model.PlanSolo, model.PlanCompany:
			// A paying plan's share follows its period's balance: without
			// one, it's the flat share.
			if a.PeriodMicroUSD != nil && *a.PeriodMicroUSD > 0 {
				plan = model.Plan{Kind: a.Kind, PeriodMicroUSD: *a.PeriodMicroUSD}
			}
		}
	}
	return *out.Allowed, out.Why, plan, nil
}

// AllowanceChecker is the runtime's runner.Runner.Allowance: it asks KIFF
// with the agent's own key, and keeps an allowed answer for AllowanceKept
// per agent. now is the clock; nil, the system's.
func AllowanceChecker(agents registry.Agents, secrets registry.Secrets, hc *http.Client, apiURL string, now func() time.Time) func(ctx context.Context, tenant, agent string) (bool, string, model.Plan, error) {
	if now == nil {
		now = time.Now
	}
	type kept struct {
		until time.Time
		plan  model.Plan
	}
	var (
		mu      sync.Mutex
		allowed = map[string]kept{} // tenant|agent → until when an allowed answer holds, with its plan
	)
	return func(ctx context.Context, tenant, agent string) (bool, string, model.Plan, error) {
		k := tenant + "|" + agent
		mu.Lock()
		was, ok := allowed[k]
		if ok && now().Before(was.until) {
			mu.Unlock()
			return true, "", was.plan, nil
		}
		delete(allowed, k)
		mu.Unlock()
		d, err := agents.Get(ctx, tenant, agent)
		if err != nil {
			return false, "", model.Plan{}, fmt.Errorf("kiff: allowance: the agent: %v", err)
		}
		key, err := secrets.Key(ctx, tenant, d.KeyRef)
		if err != nil {
			return false, "", model.Plan{}, fmt.Errorf("kiff: allowance: the agent's KIFF key: %v", err)
		}
		ok, why, plan, err := Allowance(ctx, hc, apiURL, key)
		if err != nil || !ok {
			return ok, why, plan, err
		}
		mu.Lock()
		allowed[k] = kept{until: now().Add(AllowanceKept), plan: plan}
		mu.Unlock()
		return true, "", plan, nil
	}
}
