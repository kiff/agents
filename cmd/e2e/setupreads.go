package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/kiff/agents/tool/kiff"
)

// setupReadsRecorded checks that Setup's reads went through KIFF
// (kiff/tafs#138, gate A): each setup_reads tool of this run's grants that
// Setup read is on KIFF's record as Setup's own call, allowed, with a
// receipt. A read that skipped KIFF would leave no such line. A run whose
// grants have no setup_reads tool has nothing to check.
func (t *test) setupReadsRecorded(ctx context.Context) error {
	var reads []string
	for _, id := range t.grants {
		g, err := t.phone.grant(ctx, id)
		if err != nil {
			return err
		}
		reads = append(reads, setupReadTools(g)...)
	}
	if len(reads) == 0 {
		t.logf("Setup's reads: none asked for, nothing to check")
		return nil
	}
	setup := kiffSession(kiff.APIURL, t.cfg.setupKey, t.tenant, nil, t.hc)
	me, err := setup.do(ctx, http.MethodGet, "/v1/me", nil)
	if err != nil || me.err() != nil {
		return fmt.Errorf("Setup's key in KIFF: %v %v", err, me.err())
	}
	agent, _ := me.Body["agent_id"].(string)
	if agent == "" {
		return fmt.Errorf("Setup's key names no agent in KIFF")
	}
	a, err := t.kiff.do(ctx, http.MethodGet, "/v1/me/activity?actor="+url.QueryEscape(agent)+"&limit=100", nil)
	if err != nil || a.err() != nil {
		return fmt.Errorf("reading Setup's calls in KIFF: %v %v", err, a.err())
	}
	var act setupActivity
	if err := json.Unmarshal([]byte(a.Raw), &act); err != nil {
		return fmt.Errorf("Setup's calls in KIFF: %w", err)
	}
	if err := readsOnRecord(act.Calls, agent, reads); err != nil {
		return err
	}
	t.logf("Setup's reads went through KIFF: each on its record, allowed, with a receipt")
	return nil
}

// setupActivity is KIFF's record of one agent's calls, as much as this
// check reads.
type setupActivity struct {
	Calls []setupCall `json:"calls"`
}

type setupCall struct {
	Actor   string `json:"actor_id"`
	Action  string `json:"action_name"`
	Outcome string `json:"outcome"`
	Receipt *struct {
		TraceID string `json:"trace_id"`
	} `json:"receipt"`
}

// setupReadTools are the names the grant gave its setup_reads tools: a
// tool's own name when Setup set one, else its vendor tool.
func setupReadTools(g grant) []string {
	var out []string
	for _, t := range g.Request.Tools {
		if !t.SetupReads {
			continue
		}
		name := t.Name
		if name == "" {
			name = t.Tool
		}
		out = append(out, name)
	}
	return out
}

// readsOnRecord says which setup_reads tool Setup read without KIFF's
// record of it: a call by agent, of that tool, allowed, with a receipt.
// Setup need not read every one; at least one must be on the record.
func readsOnRecord(calls []setupCall, agent string, reads []string) error {
	var seen []string
	for _, c := range calls {
		if c.Actor != agent || !slices.Contains(reads, c.Action) {
			continue
		}
		if c.Outcome != "allowed" {
			return fmt.Errorf("Setup's read of %s is on KIFF's record as %s, not allowed", c.Action, c.Outcome)
		}
		if c.Receipt == nil || c.Receipt.TraceID == "" {
			return fmt.Errorf("Setup's read of %s is on KIFF's record with no receipt", c.Action)
		}
		if !slices.Contains(seen, c.Action) {
			seen = append(seen, c.Action)
		}
	}
	if len(seen) == 0 {
		return fmt.Errorf("none of Setup's reads (%s) is on KIFF's record: they didn't go through KIFF", strings.Join(reads, ", "))
	}
	return nil
}
