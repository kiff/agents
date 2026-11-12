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
// -made names, and the counts in the log.
func runSweep(ctx context.Context, apiBase string, cfg config, out io.Writer) error {
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
	c, err := testaccount.Sweep(ctx, testaccount.KIFFSession(apiBase, cfg.session, tenant, pk, hc), made)
	fmt.Fprintln(out, c)
	return err
}
