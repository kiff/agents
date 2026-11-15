package main

import (
	"github.com/kiff/agents/cmd/internal/testaccount"

	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// runDisconnect is -disconnect: one grant of the test account, disconnected
// through planque's own route, signed by the owner's phone as any device's
// disconnect is (a grant an earlier run left connected).
// The grant must be the test account's: planque answers another account's
// as not found, and nothing is sent. The log is public, so it says only the
// grant's first characters, its status before and after, and its server's
// host.
func runDisconnect(ctx context.Context, apiBase, planqueBase string, cfg config, id string, out io.Writer) error {
	if cfg.session == "" || cfg.phone == "" {
		return errors.New("-disconnect: E2E_KIFF_SESSION and E2E_PHONE_KEY are required")
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
	return disconnectGrant(ctx, testaccount.NewPhone(pk, planqueBase, tenant, hc), id, out)
}

// disconnectGrant disconnects the phone's account's one grant whose id
// starts with prefix, and reads it back: only a grant that reads as
// disconnected after is done. One already disconnected is left as it is.
// The input is a prefix, never the whole id: a dispatch's inputs are shown
// in the run's public log. No grant, or more than one, sends nothing.
func disconnectGrant(ctx context.Context, phone *testaccount.Phone, prefix string, out io.Writer) error {
	name := shortGrant(prefix)
	if len(prefix) < len("grt_")+4 || len(prefix) > len("grt_")+8 || !strings.HasPrefix(prefix, "grt_") {
		// A whole id given by mistake is refused too: nothing runs on it.
		return fmt.Errorf("give the grant's first 8 characters (grt_ and 4 more), never its whole id")
	}
	gs, err := phone.Grants(ctx)
	if err != nil {
		return fmt.Errorf("the test account's grants can't be read: %w", err)
	}
	var match []testaccount.Grant
	for _, g := range gs {
		if strings.HasPrefix(g.ID, prefix) {
			match = append(match, g)
		}
	}
	if len(match) != 1 {
		return fmt.Errorf("%d of the test account's grants start with %s: give one grant's first characters", len(match), name)
	}
	g, id := match[0], match[0].ID
	fmt.Fprintf(out, "grant %s on the test account: %s\n", name, g.Status)
	if g.Status == "disconnected" {
		return nil
	}
	if err := phone.Disconnect(ctx, id); err != nil {
		return fmt.Errorf("disconnecting grant %s: %w", name, err)
	}
	after, err := phone.Grant(ctx, id)
	if err != nil {
		return fmt.Errorf("grant %s after its disconnect: %w", name, err)
	}
	fmt.Fprintf(out, "grant %s now: %s\n", name, after.Status)
	if after.Status != "disconnected" {
		return fmt.Errorf("grant %s is %s, not disconnected", name, after.Status)
	}
	return nil
}

// shortGrant is a grant id's first characters, as the public log names it.
func shortGrant(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id + "…"
}
