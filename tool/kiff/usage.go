package kiff

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/run"
)

// Usage is one ended run's usage report (run.Reporting): what KIFF keeps for
// the run and works out its credits from, as it keeps a call's outcome. Only
// what the run itself records, never anything the model said.
type Usage struct {
	// Agent is the run's agent here (a taf's id in tafs); KIFF knows its own
	// agent from the key the report is sent with.
	Agent   string `json:"agent"`
	Session string `json:"session,omitempty"`
	// Trigger is what started the run (run.Trigger), Source what within it
	// (a schedule's or an event's id, a door's app), Requester who asked.
	Trigger   run.Trigger    `json:"trigger"`
	Source    string         `json:"source,omitempty"`
	Requester *run.Requester `json:"requester,omitempty"`
	Started   time.Time      `json:"started"`
	Ended     time.Time      `json:"ended"`
	// CostMicroUSD is what its model turns cost, in millionths of a US
	// dollar, over all its parts. A turn of an invocation that stopped on
	// our side, before it stored what it cost, isn't in it.
	CostMicroUSD int64 `json:"cost_micro_usd"`
	Turns        int   `json:"turns"`
	// Status is how it ended (COMPLETED, ERROR or CANCELLED), Stop why a
	// completed run stopped. Failed is an ERROR: it failed on our side (it
	// couldn't start, its invocation, kiff/agents or the model failed it);
	// a cap, a cancel or a refusal isn't a failure.
	Status run.Status `json:"status"`
	Stop   string     `json:"stop,omitempty"`
	Failed bool       `json:"failed"`
	// Refused and Declined count its calls KIFF refused outright and those
	// it held that the owner refused (run.Run.Refused, Declined).
	Refused  int `json:"refused,omitempty"`
	Declined int `json:"declined,omitempty"`
	// KeptUntil is when kiff/agents may delete the run under its agent's keep
	// rules; absent, it's kept until deleted. Until then the run opens.
	KeptUntil *time.Time `json:"kept_until,omitempty"`
}

// UsageOf is r's usage report.
func UsageOf(r run.Run) Usage {
	u := Usage{Agent: r.Agent, Session: r.Session, Trigger: r.Trigger, Source: r.Source, Started: r.Created.UTC(), Ended: cmp.Or(r.Ended, r.Updated).UTC(),
		CostMicroUSD: r.Cost, Turns: r.Turns, Status: r.Status, Stop: r.Stop, Failed: r.Status == run.Error, Refused: r.Refused, Declined: r.Declined}
	if r.Requester.Kind != "" {
		q := r.Requester
		u.Requester = &q
	}
	if !r.Expires.IsZero() {
		t := r.Expires.UTC()
		u.KeptUntil = &t
	}
	return u
}

// ReportUsage sends run id's usage report to KIFF with the agent's own key:
// PUT /v1/me/runs/{id}/usage. KIFF keeps a run's first report, so sending it
// again changes nothing. Any 2xx is taken. The key is sent only as the
// Authorization header and never appears in an error.
func ReportUsage(ctx context.Context, hc *http.Client, apiURL, key, id string, u Usage) error {
	if id == "" {
		return errors.New("kiff: a usage report needs its run")
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	body, err := json.Marshal(u)
	if err != nil {
		return fmt.Errorf("kiff: usage report: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		strings.TrimSuffix(apiURL, "/")+"/v1/me/runs/"+url.PathEscape(id)+"/usage", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("kiff: usage report: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("kiff: usage report for run %s: %w", id, scrub(err, key))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("kiff: usage report for run %s: %s: %s", id, resp.Status, strings.TrimSpace(scrub(errors.New(string(b)), key).Error()))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse))
	return nil
}

// UsageReporter is the runtime's run.Reporting.Report: it sends a run's
// usage report with its agent's own key. A run whose agent is gone can't be
// reported, since the key went with it.
func UsageReporter(agents registry.Agents, secrets registry.Secrets, hc *http.Client, apiURL string) func(ctx context.Context, r run.Run) error {
	return func(ctx context.Context, r run.Run) error {
		d, err := agents.Get(ctx, r.Tenant, r.Agent)
		if err != nil {
			return fmt.Errorf("kiff: usage report: the run's agent: %v", err)
		}
		key, err := secrets.Key(ctx, r.Tenant, d.KeyRef)
		if err != nil {
			return fmt.Errorf("kiff: usage report: the agent's KIFF key: %v", err)
		}
		return ReportUsage(ctx, hc, apiURL, key, r.ID, UsageOf(r))
	}
}
