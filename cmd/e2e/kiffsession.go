package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// The owner in KIFF is a phone's session, as the tafs app's: KIFF's device
// sign-in, approved once in a browser by someone on the test account, binds
// a session to the test phone's key for 90 days. Its setup calls are signed
// with that key (KIFF-Device-Signature over kiff.request.v1), as tafs signs
// them (kiff/tafs ios/Tafs/KIFF/KIFFClient.swift).

// signedPaths are the calls KIFF takes from a phone's session only signed.
var signedPaths = []string{"/v1/keys", "/v1/me/agent-drafts"}

func signedPath(method, path string) bool {
	draft := strings.HasPrefix(path, "/v1/me/agent-drafts/")
	switch method {
	case http.MethodPost:
		return slices.Contains(signedPaths, path) || draft && strings.HasSuffix(path, "/confirm") ||
			// Archiving a taf at the end of a run (kiff/agents#78, PhoneSignedRules).
			strings.HasPrefix(path, "/v1/me/agents/") && strings.HasSuffix(path, "/archive")
	case http.MethodDelete:
		// Going back from trying a version (kiff/tafs#173).
		return draft && strings.HasSuffix(path, "/trial")
	}
	return false
}

// kiffSession is KIFF's API as the owner's phone calls it.
func kiffSession(base, token, tenant string, key *ecdsa.PrivateKey, hc *http.Client) *client {
	return &client{base: strings.TrimRight(base, "/"), http: hc, auth: func(req *http.Request, body []byte) {
		req.Header.Set("Authorization", "Bearer "+token)
		if !signedPath(req.Method, req.URL.Path) {
			return
		}
		at := time.Now().UTC().Format(time.RFC3339)
		sum := sha256.Sum256(body)
		msg := strings.Join([]string{"kiff.request.v1", tenant, req.Method, req.URL.Path + query(req), hex.EncodeToString(sum[:]), at}, "\n")
		digest := sha256.Sum256([]byte(msg))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
		req.Header.Set("KIFF-Device-Signature", "t="+at+",v1="+base64.StdEncoding.EncodeToString(sig))
	}}
}

func query(req *http.Request) string {
	if req.URL.RawQuery == "" {
		return ""
	}
	return "?" + req.URL.RawQuery
}

// kiffSignIn is the one-time setup: KIFF's device sign-in with the phone's key,
// approved by a person in their browser, then the setup agent, made through
// a draft with no tools that the owner's own phone confirms by submitting
// it. It writes the session's token and the setup agent's key to out (two
// lines, 0600) for the test's secrets, and prints neither.
func kiffSignIn(ctx context.Context, apiBase string, key *ecdsa.PrivateKey, setupAgent string, hc *http.Client, out io.Writer, say io.Writer) error {
	token, tenant, err := deviceSignIn(ctx, apiBase, key, "kiff/agents e2e (#91)", hc, say)
	if err != nil {
		return err
	}
	fmt.Fprintln(say, "signed in to KIFF; the session is bound to the test phone's key")

	k := kiffSession(apiBase, token, tenant, key, hc)
	d, err := k.do(ctx, http.MethodPost, "/v1/me/agent-drafts", map[string]any{"agent_id": setupAgent, "label": "Setup (e2e)", "cards": []any{}, "no_tools": true})
	if err != nil || d.err() != nil {
		return fmt.Errorf("the setup agent's draft: %v %v", err, d.err())
	}
	id, _ := d.Body["id"].(string)
	secret, _ := d.Body["claim_secret"].(string)
	if st, _ := d.Body["status"].(string); st != "confirmed" {
		return fmt.Errorf("the setup agent's draft is %q, not confirmed on submit", st)
	}
	claim := &client{base: strings.TrimRight(apiBase, "/"), http: hc,
		auth: func(req *http.Request, _ []byte) { req.Header.Set("Authorization", "Bearer "+secret) }}
	c, err := claim.do(ctx, http.MethodPost, "/v1/agent-drafts/"+id+"/claim", map[string]any{})
	if err != nil || c.err() != nil {
		return fmt.Errorf("claiming the setup agent's key: %v %v", err, c.err())
	}
	setupKey, _ := c.Body["plaintext"].(string)
	if roles, _ := c.Body["roles"].([]any); len(roles) != 0 {
		return fmt.Errorf("the setup agent's key holds roles %v; it should hold none", roles)
	}
	fmt.Fprintf(say, "the setup agent %s has its key, with no tools\n", setupAgent)
	_, err = fmt.Fprintf(out, "%s\n%s\n", token, setupKey)
	return err
}

// devicePoll is how often deviceSignIn asks whether the person approved.
var devicePoll = 5 * time.Second

// deviceSignIn is KIFF's device sign-in with key, approved by a person in
// their browser within 10 minutes: the session's token and the account it is
// on. It prints the link to approve, never the token.
func deviceSignIn(ctx context.Context, apiBase string, key *ecdsa.PrivateKey, deviceName string, hc *http.Client, say io.Writer) (token, tenant string, err error) {
	ph := &phone{key: key}
	anon := &client{base: strings.TrimRight(apiBase, "/"), http: hc}
	a, err := anon.do(ctx, http.MethodPost, "/v1/auth/device/start", map[string]string{"public_key": ph.publicKey(), "device_name": deviceName})
	if err != nil || a.err() != nil {
		return "", "", fmt.Errorf("starting the device sign-in: %v %v", err, a.err())
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
		a, err := anon.do(ctx, http.MethodPost, "/v1/auth/device/token", map[string]string{"device_code": deviceCode})
		if err != nil {
			return "", "", err
		}
		if a.Status == http.StatusOK {
			token, _ = a.Body["access_token"].(string)
			tenant, _ = a.Body["tenant"].(string)
			return token, tenant, nil
		}
		if e, _ := a.Body["error"].(string); e != "authorization_pending" && e != "slow_down" {
			return "", "", fmt.Errorf("the device sign-in: %w", a.err())
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
	me, err := kiffSession(apiBase, token, tenant, key, hc).do(ctx, http.MethodGet, "/v1/me", nil)
	if err != nil || me.err() != nil {
		return fmt.Errorf("KIFF didn't take %s's new session: %v %v", person, err, me.err())
	}
	name, _ := me.Body["tenant_display_name"].(string)
	role, _ := me.Body["member_role"].(string)
	id, _ := me.Body["tenant_id"].(string)
	if tenant != account || id != account {
		// Approved on another account (the person's own, say), with whatever
		// role: nothing is written.
		return fmt.Errorf("%s signed in to %q, not to the shared account: approve on that one", person, name)
	}
	if role != want {
		return fmt.Errorf("%s signed in to %q as %q, not as %s", person, name, role, want)
	}
	fmt.Fprintf(say, "%s is signed in to %q as %s; the session is bound to a new phone key\n", person, name, role)
	_, err = fmt.Fprintf(out, "%s\n%s\n", base64.StdEncoding.EncodeToString(der), token)
	return err
}
