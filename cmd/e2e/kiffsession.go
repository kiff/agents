package main

import (
	"github.com/kiff/agents/cmd/internal/testaccount"

	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// kiffSignIn is the one-time setup of the test's own account: a new phone
// key, KIFF's device sign-in with it, approved by a person in their browser on
// that account (whose id is account), then the setup agent, made through a
// draft with no tools that the phone confirms by submitting it. The test's
// account is its own: people use none of what it does, so neither its holds'
// alerts nor its grants reach anyone. It writes the session's token, the setup
// agent's key and the phone key (base64 PKCS#8) to out (three lines, 0600) for
// the test's secrets, and prints none of them. Approved on any other account,
// it writes nothing.
func kiffSignIn(ctx context.Context, apiBase, account, setupAgent string, hc *http.Client, out io.Writer, say io.Writer) error {
	if account == "" {
		return errors.New("-account: the test's own account's id")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	token, tenant, err := deviceSignIn(ctx, apiBase, key, "kiff/agents e2e (#91)", hc, say)
	if err != nil {
		return err
	}
	if tenant != account {
		signOut(ctx, apiBase, token, hc, say)
		return errors.New("the sign-in was approved on another account, not the test's own: approve on that one")
	}
	fmt.Fprintln(say, "signed in to KIFF; the session is bound to the test phone's key")

	k := testaccount.KIFFSession(apiBase, token, tenant, key, hc)
	d, err := k.Do(ctx, http.MethodPost, "/v1/me/agent-drafts", map[string]any{"agent_id": setupAgent, "label": "Setup (e2e)", "cards": []any{}, "no_tools": true})
	if err != nil || d.Err() != nil {
		return fmt.Errorf("the setup agent's draft: %v %v", err, d.Err())
	}
	id, _ := d.Body["id"].(string)
	secret, _ := d.Body["claim_secret"].(string)
	if st, _ := d.Body["status"].(string); st != "confirmed" {
		return fmt.Errorf("the setup agent's draft is %q, not confirmed on submit", st)
	}
	claim := &client{Base: strings.TrimRight(apiBase, "/"), HTTP: hc,
		Auth: func(req *http.Request, _ []byte) { req.Header.Set("Authorization", "Bearer "+secret) }}
	c, err := claim.Do(ctx, http.MethodPost, "/v1/agent-drafts/"+id+"/claim", map[string]any{})
	if err != nil || c.Err() != nil {
		return fmt.Errorf("claiming the setup agent's key: %v %v", err, c.Err())
	}
	setupKey, _ := c.Body["plaintext"].(string)
	if roles, _ := c.Body["roles"].([]any); len(roles) != 0 {
		return fmt.Errorf("the setup agent's key holds roles %v; it should hold none", roles)
	}
	fmt.Fprintf(say, "the setup agent %s has its key, with no tools\n", setupAgent)
	_, err = fmt.Fprintf(out, "%s\n%s\n%s\n", token, setupKey, base64.StdEncoding.EncodeToString(der))
	return err
}

// signOut ends a session the test won't keep, as the app's sign out does: one
// approved on the wrong account, or with the wrong role, would otherwise stay
// live there for its 90 days, listed as a device.
func signOut(ctx context.Context, apiBase, token string, hc *http.Client, say io.Writer) {
	c := &client{Base: strings.TrimRight(apiBase, "/"), HTTP: hc,
		Auth: func(req *http.Request, _ []byte) { req.Header.Set("Authorization", "Bearer "+token) }}
	if a, err := c.Do(ctx, http.MethodPost, "/v1/auth/device/logout", nil); err != nil || a.Err() != nil {
		fmt.Fprintf(say, "the session couldn't be signed out (%v %v): remove the device in KIFF\n", err, a.Err())
		return
	}
	fmt.Fprintln(say, "the session is signed out")
}

// devicePoll is how often deviceSignIn asks whether the person approved.
var devicePoll = 5 * time.Second

// deviceSignIn is KIFF's device sign-in with key, approved by a person in
// their browser within 10 minutes: the session's token and the account it is
// on. It prints the link to approve, never the token.
func deviceSignIn(ctx context.Context, apiBase string, key *ecdsa.PrivateKey, deviceName string, hc *http.Client, say io.Writer) (token, tenant string, err error) {
	ph := &phone{Key: key}
	anon := &client{Base: strings.TrimRight(apiBase, "/"), HTTP: hc}
	a, err := anon.Do(ctx, http.MethodPost, "/v1/auth/device/start", map[string]string{"public_key": ph.PublicKey(), "device_name": deviceName})
	if err != nil || a.Err() != nil {
		return "", "", fmt.Errorf("starting the device sign-in: %v %v", err, a.Err())
	}
	deviceCode, _ := a.Body["device_code"].(string)
	link, _ := a.Body["verification_uri_complete"].(string)
	fmt.Fprintf(say, "Approve this sign-in as the person, on the KIFF account it's for, within 10 minutes:\n  %s\n", link)
	for deadline := time.Now().Add(10 * time.Minute); time.Now().Before(deadline); {
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(devicePoll):
		}
		a, err := anon.Do(ctx, http.MethodPost, "/v1/auth/device/token", map[string]string{"device_code": deviceCode})
		if err != nil {
			return "", "", err
		}
		if a.Status == http.StatusOK {
			token, _ = a.Body["access_token"].(string)
			tenant, _ = a.Body["tenant"].(string)
			return token, tenant, nil
		}
		if e, _ := a.Body["error"].(string); e != "authorization_pending" && e != "slow_down" {
			return "", "", fmt.Errorf("the device sign-in: %w", a.Err())
		}
	}
	return "", "", errors.New("nobody approved the sign-in in time")
}

// stepNine is each of step 9's people and the role they hold on the shared
// account: Maria an editor who manages Support, Luis a
// viewer in it.
var stepNine = map[string]string{"maria": "editor", "luis": "viewer"}

// peopleSignIn signs one of step 9's people in, as their own phone would
// (kiff/tafs#294): a new phone key and KIFF's device sign-in, which that
// person approves on the shared account, whose id is account. It makes no
// agent: the person only uses what is shared with them. It writes the phone
// key (base64 PKCS#8) and the session's token to out (two lines, 0600), for
// a script that puts them in the repository's secrets, and prints neither:
// only the account and the role KIFF gives the session, to check by eye.
func peopleSignIn(ctx context.Context, apiBase, account, person string, hc *http.Client, out io.Writer, say io.Writer) error {
	want, ok := stepNine[person]
	if !ok {
		return fmt.Errorf("-person %q: maria or luis", person)
	}
	if account == "" {
		return errors.New("-account: the shared account's id")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	token, tenant, err := deviceSignIn(ctx, apiBase, key, "kiff/agents e2e, "+person, hc, say)
	if err != nil {
		return err
	}
	me, err := testaccount.KIFFSession(apiBase, token, tenant, key, hc).Do(ctx, http.MethodGet, "/v1/me", nil)
	if err != nil || me.Err() != nil {
		return fmt.Errorf("KIFF didn't take %s's new session: %v %v", person, err, me.Err())
	}
	name, _ := me.Body["tenant_display_name"].(string)
	role, _ := me.Body["member_role"].(string)
	id, _ := me.Body["tenant_id"].(string)
	if tenant != account || id != account {
		// Approved on another account (the person's own, say), with whatever
		// role: nothing is written, and the session is signed out.
		signOut(ctx, apiBase, token, hc, say)
		return fmt.Errorf("%s signed in to %q, not to the shared account: approve on that one", person, name)
	}
	if role != want {
		signOut(ctx, apiBase, token, hc, say)
		return fmt.Errorf("%s signed in to %q as %q, not as %s", person, name, role, want)
	}
	fmt.Fprintf(say, "%s is signed in to %q as %s; the session is bound to a new phone key\n", person, name, role)
	_, err = fmt.Fprintf(out, "%s\n%s\n", base64.StdEncoding.EncodeToString(der), token)
	return err
}
