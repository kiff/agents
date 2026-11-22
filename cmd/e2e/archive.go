package main

import (
	"github.com/kiff/agents/cmd/internal/testaccount"

	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// runArchive is -archive: one taf of the test account, archived as the
// owner's phone, as the sweep archives the ones a run recorded. It's for a
// taf a run made but never recorded (a run cut off between making it and
// recording it), which the sweep can't know.
func runArchive(ctx context.Context, apiBase string, cfg config, prefix string, out io.Writer) error {
	if cfg.session == "" || cfg.phone == "" {
		return errors.New("-archive: E2E_KIFF_SESSION and E2E_PHONE_KEY are required")
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
	return archiveAgent(ctx, testaccount.KIFFSession(apiBase, cfg.session, tenant, pk, hc), prefix, out)
}

// archiveAgent archives the test account's one agent whose id starts with
// prefix, and reads the account's access back: only an agent that's gone
// from it after is done. Setup's agent is the account's and is never
// archived here. The input is a prefix, never the whole id, because a
// dispatch's inputs show in the run's public log. No agent, or more than
// one, sends nothing.
func archiveAgent(ctx context.Context, kiffc *testaccount.Client, prefix string, out io.Writer) error {
	name := shortAgent(prefix)
	// 8 to 12 characters of the id: agt_ and 4 to 8 more, or the start of
	// an older taf's id, which KIFF named by its job ("refunds-…"). A whole
	// id given by mistake is refused too: nothing runs on it.
	if len(prefix) < 8 || len(prefix) > 12 || strings.ContainsAny(prefix, " /?#") {
		return fmt.Errorf("give the taf's first 8 to 12 characters (agt_ and 4 more, or an older id's start), never its whole id")
	}
	there, setup, err := testaccount.PhoneAccess(ctx, kiffc)
	if err != nil {
		return fmt.Errorf("the test account's tafs can't be read: %w", err)
	}
	var match []string
	for id := range there {
		if id == prefix {
			return fmt.Errorf("give the taf's first characters, never its whole id")
		}
		if strings.HasPrefix(id, prefix) {
			match = append(match, id)
		}
	}
	if len(match) != 1 {
		return fmt.Errorf("%d of the test account's tafs start with %s: give one taf's first characters", len(match), name)
	}
	id := match[0]
	if id == setup {
		return fmt.Errorf("%s is Setup's agent, the account's own: it's never archived here", name)
	}
	// Whatever is said from here goes to a public log: the whole id never,
	// in this command's words or in an error it passes on.
	scrub := func(err error) error {
		if err == nil {
			return nil
		}
		return errors.New(strings.ReplaceAll(err.Error(), id, name))
	}
	ans, err := kiffc.Do(ctx, http.MethodPost, "/v1/me/agents/"+url.PathEscape(id)+"/archive", map[string]string{})
	switch {
	case err != nil:
		return scrub(fmt.Errorf("archiving taf %s: %w", name, err))
	case ans.Status == http.StatusNotFound:
		// The prune took it between the read and the call: the read below says.
	case ans.Err() != nil:
		return scrub(fmt.Errorf("archiving taf %s: %w", name, ans.Err()))
	}
	if there, _, err = testaccount.PhoneAccess(ctx, kiffc); err != nil {
		return scrub(fmt.Errorf("taf %s after its archive: %w", name, err))
	}
	if there[id] {
		return fmt.Errorf("taf %s is still in the test account's access", name)
	}
	fmt.Fprintf(out, "taf %s archived: it has left the test account's access\n", name)
	return nil
}

// shortAgent is an agent id's first characters, as the public log names it.
func shortAgent(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id + "…"
}
