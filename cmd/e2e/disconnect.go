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

// disconnectGrant disconnects the phone's account's grant id and reads it
// back: only a grant that reads as disconnected after is done. One already
// disconnected is left as it is.
func disconnectGrant(ctx context.Context, phone *testaccount.Phone, id string, out io.Writer) error {
	name := shortGrant(id)
	g, err := phone.Grant(ctx, id)
	if err != nil {
		return fmt.Errorf("grant %s isn't the test account's, or can't be read: %w", name, err)
	}
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
	return id
}
