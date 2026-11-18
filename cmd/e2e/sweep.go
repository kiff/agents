package main

import (
	"github.com/kiff/agents/cmd/internal/testaccount"

	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// runSweep is -sweep: the phone's session in KIFF at apiBase, the tafs
// -made names, and the counts in the log. After archiving them, it ends the
// grants they left unfinished in planque at planqueBase: a run that stops
// half-way (a sign-in that failed after the phone approved) would otherwise
// leave an approved grant on the test account for the next run's taf of the
// same name to pick up.
func runSweep(ctx context.Context, apiBase, planqueBase string, cfg config, out io.Writer) error {
	if cfg.session == "" || cfg.phone == "" {
		return errors.New("-sweep: E2E_KIFF_SESSION and E2E_PHONE_KEY are required")
	}
	pk, err := testaccount.ParsePhone(cfg.phone)
	if err != nil {
		return err
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	me, err := testaccount.KIFFSession(apiBase, cfg.session, "", pk, hc).Do(ctx, http.MethodGet, "/v1/me", nil)
	if err != nil || me.Err() != nil {
		return fmt.Errorf("KIFF didn't take the phone's session: %v %v", err, me.Err())
	}
	tenant, _ := me.Body["tenant_id"].(string)
	made, err := testaccount.ReadMade(cfg.made)
	if err != nil {
		return err
	}
	kiffc := testaccount.KIFFSession(apiBase, cfg.session, tenant, pk, hc)
	_, setup, accessErr := testaccount.PhoneAccess(ctx, kiffc)
	c, swept := testaccount.Sweep(ctx, kiffc, made)
	fmt.Fprintln(out, c)
	if accessErr != nil {
		// Without Setup's agent, no grant is ended: Setup's own must never be.
		return errors.Join(swept, fmt.Errorf("the sweep ended no grants: %w", accessErr))
	}
	phone := testaccount.NewPhone(pk, planqueBase, tenant, hc)
	phone.Device, err = testaccount.ThisDevice(ctx, kiffc)
	if err == nil {
		var gs []testaccount.Grant
		if gs, err = everyGrant(ctx, phone); err == nil {
			g := endTheirGrants(gs, theirs(made, setup), func(id string) error { return phone.Disconnect(ctx, id) })
			fmt.Fprintln(out, g)
			err = g.err()
		}
	}
	if err != nil {
		err = fmt.Errorf("the sweep's grants: %w", err)
	}
	return errors.Join(swept, err)
}

// theirs is the agents whose grants the sweep may end: the ones this job
// made, never Setup's, which is the account's.
func theirs(made []string, setup string) map[string]bool {
	m := make(map[string]bool, len(made))
	for _, a := range made {
		if a != "" && a != setup {
			m[a] = true
		}
	}
	return m
}

// finished says planque is done with a grant: nothing more to end.
func finished(status string) bool { return status == "disconnected" || status == "failed" }

// grantSweep is what the sweep did to the grants, for the public log: counts only.
type grantSweep struct{ ended, failed, others int }

func (g grantSweep) String() string {
	return fmt.Sprintf("sweep: %d unfinished grants of those tafs ended, %d failed; %d other grants left as they are", g.ended, g.failed, g.others)
}

func (g grantSweep) err() error {
	if g.failed > 0 {
		return fmt.Errorf("%d of the sweep's grants couldn't be ended", g.failed)
	}
	return nil
}

// endTheirGrants ends, through disconnect, every grant of one of agents that
// planque hasn't finished: pending, approved, signed in, listed or connected.
// Any other agent's grant, and a finished one, is left as it is.
func endTheirGrants(gs []testaccount.Grant, agents map[string]bool, disconnect func(id string) error) grantSweep {
	var g grantSweep
	for _, gr := range gs {
		if !agents[gr.Request.Agent] {
			if !finished(gr.Status) {
				g.others++
			}
			continue
		}
		if finished(gr.Status) {
			continue
		}
		if err := disconnect(gr.ID); err != nil {
			g.failed++
			continue
		}
		g.ended++
	}
	return g
}
