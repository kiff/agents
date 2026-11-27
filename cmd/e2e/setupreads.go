package main

import (
	"github.com/kiff/agents/cmd/internal/testaccount"

	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool/kiff"
	"github.com/kiff/agents/tool/planque"
)

// setupReadsRecorded checks that Setup's reads went through KIFF
// (kiff/tafs#138, gate A): each setup_reads tool of this run's grants that
// Setup read is on KIFF's record as Setup's own call, allowed, with a
// receipt. A read that skipped KIFF would leave no such line. A run whose
// grants have no setup_reads tool has nothing to check.
//
// Which reads Setup asked for comes from its own chat (its planque calls
// with action "read", readsAsked): a read that skipped KIFF is still one of
// those. So a run where Setup read nothing has nothing to check, and each
// read it did ask for must be on KIFF's record. Without the chat (none
// read), at least one of the setup_reads tools must be on the record.
func (t *test) setupReadsRecorded(ctx context.Context) error {
	var reads []string
	for _, id := range t.grants {
		g, err := t.phone.Grant(ctx, id)
		if err != nil {
			return err
		}
		reads = append(reads, setupReadTools(g)...)
	}
	if len(reads) == 0 {
		t.logf("Setup's reads: none asked for, nothing to check")
		return nil
	}
	if t.setupSession != "" {
		if _, events, err := t.sessions.Read(ctx, t.tenant, t.setupSession); err == nil {
			asked := readsAsked(events, reads)
			if len(asked) == 0 {
				t.logf("Setup's reads: Setup read none of %s in its chat, nothing to check", strings.Join(reads, ", "))
				return nil
			}
			reads = asked
		}
	}
	setup := testaccount.KIFFSession(kiff.APIURL, t.cfg.setupKey, t.tenant, nil, t.hc)
	me, err := setup.Do(ctx, http.MethodGet, "/v1/me", nil)
	if err != nil || me.Err() != nil {
		return fmt.Errorf("Setup's key in KIFF: %v %v", err, me.Err())
	}
	agent, _ := me.Body["agent_id"].(string)
	if agent == "" {
		return fmt.Errorf("Setup's key names no agent in KIFF")
	}
	a, err := t.kiff.Do(ctx, http.MethodGet, "/v1/me/activity?actor="+url.QueryEscape(agent)+"&limit=100", nil)
	if err != nil || a.Err() != nil {
		return fmt.Errorf("reading Setup's calls in KIFF: %v %v", err, a.Err())
	}
	var act setupActivity
	if err := json.Unmarshal([]byte(a.Raw), &act); err != nil {
		return fmt.Errorf("Setup's calls in KIFF: %w", err)
	}
	if err := readsOnRecord(act.Calls, agent, reads, t.setupSession != ""); err != nil {
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
// With every (the reads Setup asked for, from its chat), each must be on the
// record; without, Setup need not have read every one, and at least one must
// be.
func readsOnRecord(calls []setupCall, agent string, reads []string, every bool) error {
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
	if every {
		for _, r := range reads {
			if !slices.Contains(seen, r) {
				return fmt.Errorf("Setup read %s in its chat, and it isn't on KIFF's record: the read didn't go through KIFF", r)
			}
		}
		return nil
	}
	if len(seen) == 0 {
		// KIFF's record can't tell a read that skipped it from no read at all (a run whose draft was refused before
		// Setup read anything): the message says both, and how many other calls of Setup's KIFF has.
		others := 0
		for _, c := range calls {
			if c.Actor == agent {
				others++
			}
		}
		return fmt.Errorf("none of Setup's reads (%s) is on KIFF's record, which has %d other calls by Setup: Setup made no read, or its reads didn't go through KIFF", strings.Join(reads, ", "), others)
	}
	return nil
}

// readsAsked are the setup_reads tools Setup asked planque to read in its
// chat: its planque calls with action "read", as its own session recorded
// them, whatever came of them. A read that skipped KIFF is one of these too.
func readsAsked(events []session.Event, reads []string) []string {
	var asked []string
	for _, e := range events {
		if e.Kind != session.Called {
			continue
		}
		var b agent.CalledBody
		if json.Unmarshal(e.Body, &b) != nil || b.Tool != planque.Name {
			continue
		}
		var in struct {
			Action string `json:"action"`
			Tool   string `json:"tool"`
		}
		if json.Unmarshal(b.Input, &in) != nil || in.Action != "read" || !slices.Contains(reads, in.Tool) {
			continue
		}
		if !slices.Contains(asked, in.Tool) {
			asked = append(asked, in.Tool)
		}
	}
	return asked
}
