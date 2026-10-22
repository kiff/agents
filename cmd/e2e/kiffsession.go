package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
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
	if method != http.MethodPost {
		return false
	}
	return slices.Contains(signedPaths, path) ||
		strings.HasPrefix(path, "/v1/me/agent-drafts/") && strings.HasSuffix(path, "/confirm") ||
		// Archiving a taf at the end of a run (kiff/agents#78, PhoneSignedRules).
		strings.HasPrefix(path, "/v1/me/agents/") && strings.HasSuffix(path, "/archive")
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
	ph := &phone{key: key}
	anon := &client{base: strings.TrimRight(apiBase, "/"), http: hc}
	a, err := anon.do(ctx, http.MethodPost, "/v1/auth/device/start", map[string]string{"public_key": ph.publicKey(), "device_name": "kiff/agents e2e (#91)"})
	if err != nil || a.err() != nil {
		return fmt.Errorf("starting the device sign-in: %v %v", err, a.err())
	}
	deviceCode, _ := a.Body["device_code"].(string)
	link, _ := a.Body["verification_uri_complete"].(string)
	fmt.Fprintf(say, "Approve this sign-in as someone on the KIFF test account, within 10 minutes:\n  %s\n", link)
	var token, tenant string
	for deadline := time.Now().Add(10 * time.Minute); time.Now().Before(deadline); {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
		a, err := anon.do(ctx, http.MethodPost, "/v1/auth/device/token", map[string]string{"device_code": deviceCode})
		if err != nil {
			return err
		}
		if a.Status == http.StatusOK {
			token, _ = a.Body["access_token"].(string)
			tenant, _ = a.Body["tenant"].(string)
			break
		}
		if e, _ := a.Body["error"].(string); e != "authorization_pending" && e != "slow_down" {
			return fmt.Errorf("the device sign-in: %w", a.err())
		}
	}
	if token == "" {
		return errors.New("nobody approved the sign-in in time")
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
