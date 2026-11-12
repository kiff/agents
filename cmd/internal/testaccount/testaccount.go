// Package testaccount is the test account as the end-to-end test and the
// Eval both reach it: a JSON client, the owner's phone toward planque and
// KIFF (signed as the tafs app signs), its grants and keys, and the sweep
// that archives what a run made.
package testaccount

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

// client is a JSON client for one service, with a way to authenticate each
// request.
type Client struct {
	Base string
	Auth func(req *http.Request, body []byte)
	HTTP *http.Client
}

// answer is a JSON answer, with its status.
type Answer struct {
	Status int
	Body   map[string]any
	Raw    string
}

func (a Answer) Err() error {
	if a.Status >= 200 && a.Status < 300 {
		return nil
	}
	msg, _ := a.Body["error"].(string)
	if m, _ := a.Body["message"].(string); m != "" {
		msg = m
	}
	return fmt.Errorf("%d: %s", a.Status, msg)
}

func (c *Client) Do(ctx context.Context, method, path string, body any) (Answer, error) {
	return c.DoWith(ctx, method, path, body, nil)
}

// doWith is do, with headers set outside the signed body: an API key goes
// to planque this way, the body carrying only its SHA-256.
func (c *Client) DoWith(ctx context.Context, method, path string, body any, header http.Header) (Answer, error) {
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			return Answer{}, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, bytes.NewReader(b))
	if err != nil {
		return Answer{}, err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range header {
		req.Header[k] = v
	}
	if b != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Auth != nil {
		c.Auth(req, b)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Answer{}, err
	}
	defer resp.Body.Close()
	// Read whole: an answer cut short decodes as nothing, and a grant that
	// was there reads as missing. An account's grants list grows with every
	// run (kiff/tafs#214).
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxAnswer+1))
	if err != nil {
		return Answer{}, err
	}
	if len(raw) > MaxAnswer {
		return Answer{}, fmt.Errorf("%s %s: the answer is over %d bytes", method, path, MaxAnswer)
	}
	a := Answer{Status: resp.StatusCode, Raw: string(raw)}
	_ = json.Unmarshal(raw, &a.Body)
	return a, nil
}

// phone is the owner's phone toward planque: a P-256 key, as tafs'
// ApprovalSigner, signing each call (contract 2 on kiff/planque#1).
type Phone struct {
	Key     *ecdsa.PrivateKey
	Planque *Client
	Tenant  string
	// device is this phone's id in KIFF, sent with each signature (d=), so
	// planque asks KIFF whether it signed, as the tafs app does
	// (kiff/planque#12). Empty signs with the key alone.
	Device string
	// signIn completes the vendor's sign-in and returns the code from the
	// app's link: on its own at the fake vendor, by a person at Stripe.
	SignIn func(ctx context.Context, link string) (string, error)
	// vendorKey is the key an API-key grant's approval gives (E2E_DESK_KEY).
	VendorKey string
}

// parsePhone reads a base64 PKCS#8 P-256 key: the test's stable phone, so
// planque keeps it enrolled from one night to the next.
func ParsePhone(b64 string) (*ecdsa.PrivateKey, error) {
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, errors.New("E2E_PHONE_KEY is base64 PKCS#8")
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("E2E_PHONE_KEY is not an ECDSA key")
	}
	return ec, nil
}

func NewPhone(key *ecdsa.PrivateKey, planqueURL, tenant string, hc *http.Client) *Phone {
	p := &Phone{Key: key, Tenant: tenant, SignIn: SignIn}
	base := strings.TrimRight(planqueURL, "/")
	p.Planque = &Client{Base: base, HTTP: hc, Auth: func(req *http.Request, body []byte) {
		// To the nanosecond: planque refuses a signed request made twice, so
		// two reads in one second must differ.
		at := time.Now().UTC().Format(time.RFC3339Nano)
		sum := sha256.Sum256(body)
		msg := strings.Join([]string{"planque.request.v1", base, tenant, req.Method, req.URL.RequestURI(),
			hex.EncodeToString(sum[:]), at}, "\n")
		digest := sha256.Sum256([]byte(msg))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
		req.Header.Set("Planque-Device-Signature", SignatureHeader(at, sig, p.Device))
	}}
	return p
}

// signatureHeader is planque's device signature header: the time, the
// signature and, for a device KIFF knows, its id.
func SignatureHeader(at string, sig []byte, device string) string {
	h := "t=" + at + ",v1=" + base64.StdEncoding.EncodeToString(sig)
	if device != "" {
		h += ",d=" + device
	}
	return h
}

// thisDevice is the phone's id in KIFF: the device its session signed in
// as (GET /v1/me/devices, this_device), as the tafs app reads it.
func ThisDevice(ctx context.Context, kiff *Client) (string, error) {
	a, err := kiff.Do(ctx, http.MethodGet, "/v1/me/devices", nil)
	if err != nil {
		return "", err
	}
	if err := a.Err(); err != nil {
		return "", err
	}
	devices, _ := a.Body["devices"].([]any)
	for _, d := range devices {
		m, _ := d.(map[string]any)
		if id, _ := m["id"].(string); m["this_device"] == true && strings.HasPrefix(id, "dev_") {
			return id, nil
		}
	}
	return "", errors.New("KIFF lists no device for this session: renew it with -signin")
}

func (p *Phone) PublicKey() string {
	der, _ := x509.MarshalPKIXPublicKey(&p.Key.PublicKey)
	return base64.StdEncoding.EncodeToString(der)
}

func (p *Phone) AccountPath() string { return "/v1/accounts/" + url.PathEscape(p.Tenant) }

// grantTool is one tool of a grant, as asked for.
type GrantTool struct {
	Tool        string         `json:"tool"`
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Fixed       map[string]any `json:"fixed_arguments"`
	ReadOnly    bool           `json:"read_only"`
	SetupReads  bool           `json:"setup_reads"`
	AmountArg   string         `json:"amount_argument"`
	ResourceArg string         `json:"resource_argument"`
	UnitLookup  *struct {
		Tool string `json:"tool"`
	} `json:"unit_lookup"`
}

// grant is a grant as the phone reads it.
type Grant struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	// SignedIn: planque holds the grant's sign-in, or its key: adding tools
	// takes an approval alone, with no second sign-in or key.
	SignedIn bool     `json:"signed_in"`
	Text     string   `json:"text"`
	Digest   string   `json:"digest"`
	Tools    []string `json:"tools"`
	Reason   string   `json:"reason"`
	// Request is what the owner approved: the tools, as asked for.
	Request struct {
		Server string      `json:"server"`
		Agent  string      `json:"agent"`
		Kind   string      `json:"kind"`
		Tools  []GrantTool `json:"tools"`
	} `json:"request"`
	// Events is the grant's record, such as "read list_accounts at …" when
	// Setup read what the sign-in covers (kiff/planque#14).
	Events []struct {
		What string `json:"what"`
	} `json:"events"`
	Raw json.RawMessage `json:"-"`
}

// MaxAnswer bounds an answer the e2e reads.
const MaxAnswer = 32 << 20

func (p *Phone) Grants(ctx context.Context) ([]Grant, error) {
	a, err := p.Planque.Do(ctx, http.MethodGet, p.AccountPath()+"/grants", nil)
	if err != nil {
		return nil, err
	}
	if err := a.Err(); err != nil {
		return nil, fmt.Errorf("planque grants: %w", err)
	}
	var out struct {
		Grants []Grant `json:"grants"`
	}
	if err := json.Unmarshal([]byte(a.Raw), &out); err != nil {
		return nil, fmt.Errorf("planque grants: %w", err)
	}
	return out.Grants, nil
}

func (p *Phone) Grant(ctx context.Context, id string) (Grant, error) {
	gs, err := p.Grants(ctx)
	if err != nil {
		return Grant{}, err
	}
	for _, g := range gs {
		if g.ID == id {
			return g, nil
		}
	}
	return Grant{}, fmt.Errorf("planque has no grant %s for this account", id)
}

// approveAndSignIn is the owner approving a grant on the phone, signing in
// at the vendor in a browser that follows redirects up to the app's return
// link, and the phone confirming with the code that link carries. A grant
// whose sign-in planque already holds (its tools set after connecting
// first) connects on approval, with no sign-in. It returns the status.
func (p *Phone) ApproveAndSignIn(ctx context.Context, g Grant) (string, error) {
	if g.Request.Kind == "api_key" && !g.SignedIn {
		return p.GiveKey(ctx, g)
	}
	a, err := p.Planque.Do(ctx, http.MethodPost, p.AccountPath()+"/grants/"+url.PathEscape(g.ID)+"/approve", map[string]string{"digest": g.Digest})
	if err != nil {
		return "", err
	}
	if err := a.Err(); err != nil {
		return "", fmt.Errorf("approve: %w", err)
	}
	if st, _ := a.Body["status"].(string); st == "connected" {
		return "connected, with no second sign-in", nil
	}
	link, _ := a.Body["sign_in_url"].(string)
	code, err := p.SignIn(ctx, link)
	if err != nil {
		return "", err
	}
	a, err = p.Planque.Do(ctx, http.MethodPost, p.AccountPath()+"/grants/"+url.PathEscape(g.ID)+"/confirm", map[string]string{"confirm": code})
	if err != nil {
		return "", err
	}
	if err := a.Err(); err != nil {
		return "", fmt.Errorf("confirm: %w", err)
	}
	switch st, _ := a.Body["status"].(string); st {
	case "connected":
		return "signed in at the vendor, confirmed: connected", nil
	case "listed":
		return "signed in at the vendor, confirmed: listed (the vendor's tools, for Setup to choose from)", nil
	default:
		return "", fmt.Errorf("after confirm the grant is %q", st)
	}
}

// signIn follows a vendor sign-in link as a browser would, until planque
// sends it to the owner's app (tafs://planque/signed-in), and returns the
// confirmation code. The fake vendor consents on its own.
func SignIn(ctx context.Context, link string) (string, error) {
	if link == "" {
		return "", errors.New("approve gave no sign-in link")
	}
	hc := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || len(via) > 10 {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Scheme != "tafs" {
		return "", fmt.Errorf("the sign-in ended at %d %q, not at the app's link", resp.StatusCode, resp.Header.Get("Location"))
	}
	code := loc.Query().Get("confirm")
	if code == "" {
		return "", errors.New("the app's link carries no confirmation code")
	}
	return code, nil
}

func (p *Phone) Disconnect(ctx context.Context, id string) error {
	a, err := p.Planque.Do(ctx, http.MethodDelete, p.AccountPath()+"/grants/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return a.Err()
}

// giveKey approves an API-key grant as the owner's phone does: it signs the
// grant's digest and the key's SHA-256, and sends the key itself in
// planque's Planque-Vendor-Key header, never in the signed body. The key is
// the test vendor's (E2E_DESK_KEY).
func (p *Phone) GiveKey(ctx context.Context, g Grant) (string, error) {
	if p.VendorKey == "" {
		return "", errors.New("an API-key grant needs E2E_DESK_KEY, the test vendor's key")
	}
	sum := sha256.Sum256([]byte(p.VendorKey))
	a, err := p.Planque.DoWith(ctx, http.MethodPost, p.AccountPath()+"/grants/"+url.PathEscape(g.ID)+"/key",
		map[string]string{"digest": g.Digest, "key_sha256": hex.EncodeToString(sum[:])},
		http.Header{"Planque-Vendor-Key": {p.VendorKey}})
	if err != nil {
		return "", err
	}
	if err := a.Err(); err != nil {
		return "", fmt.Errorf("key: %w", err)
	}
	switch st, _ := a.Body["status"].(string); st {
	case "connected":
		return "the key given on the phone: connected", nil
	case "listed":
		return "the key given on the phone: listed (the vendor's operations, for Setup to choose from)", nil
	default:
		return "", fmt.Errorf("after the key the grant is %q", st)
	}
}

// The owner in KIFF is a phone's session, as the tafs app's: KIFF's device
// sign-in, approved once in a browser by someone on the test account, binds
// a session to the test phone's key for 90 days. Its setup calls are signed
// with that key (KIFF-Device-Signature over kiff.request.v1), as tafs signs
// them (kiff/tafs ios/Tafs/KIFF/KIFFClient.swift).

// signedPaths are the calls KIFF takes from a phone's session only signed.
var SignedPaths = []string{"/v1/keys", "/v1/me/agent-drafts"}

func SignedPath(method, path string) bool {
	draft := strings.HasPrefix(path, "/v1/me/agent-drafts/")
	// Sharing a taf with a group, and unsharing it (step 9, kiff/tafs#294).
	sharing := strings.HasPrefix(path, "/v1/agents/") && strings.HasSuffix(path, "/grants")
	switch method {
	case http.MethodPost:
		return slices.Contains(SignedPaths, path) || draft && strings.HasSuffix(path, "/confirm") || sharing ||
			// Archiving a taf at the end of a run (kiff/agents#78, PhoneSignedRules).
			strings.HasPrefix(path, "/v1/me/agents/") && strings.HasSuffix(path, "/archive")
	case http.MethodDelete:
		// Going back from trying a version (kiff/tafs#173), and revoking a
		// key the phone minted.
		return draft && strings.HasSuffix(path, "/trial") || sharing || strings.HasPrefix(path, "/v1/keys/")
	}
	return false
}

// kiffSession is KIFF's API as the owner's phone calls it.
func KIFFSession(base, token, tenant string, key *ecdsa.PrivateKey, hc *http.Client) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), HTTP: hc, Auth: func(req *http.Request, body []byte) {
		req.Header.Set("Authorization", "Bearer "+token)
		if !SignedPath(req.Method, req.URL.Path) {
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

// The sweep (-sweep) is the test account's teardown, the workflow's last
// step whatever the steps before it did. A run's cleanup archives the tafs
// it made, and KIFF revokes an archived agent's keys; the account's hourly
// prune then deletes both. What a run never cleaned up stays: a run cut off
// before its cleanup. The sweep takes those, and nothing else: the tafs this
// job's runs made (-made, written as each draft is confirmed).
//
// It works as the owner's phone, so it makes only the calls KIFF takes from
// a phone's session: it reads the phone's own access (GET /v1/me/access, the
// account's agents that aren't archived, and which one is Setup's) and
// archives each taf by its id, signed. KIFF never lets a phone's session
// list the account's keys, so the sweep doesn't ask: an earlier version did,
// was refused every time (403), and swept nothing.
//
// A taf this job made that is no longer in the phone's access is already
// archived: nothing to do. Setup's agent is never archived, even if -made
// names it. Tafs left by runs before this one aren't found from a phone's
// session; whoever holds the account's dashboard removes those. The log
// says only how many: the nightly log is public.

// sweepCounts is what a sweep did, for the log.
type SweepCounts struct {
	archived, gone, failed, left int
}

func (c SweepCounts) String() string {
	return fmt.Sprintf("sweep: %d tafs archived (their keys with them), %d already gone, %d failed; %d left to sweep",
		c.archived, c.gone, c.failed, c.left)
}

// sweep archives the tafs this job's runs made, through KIFF as the owner's
// phone, and counts what is still there after.
func Sweep(ctx context.Context, kiffc *Client, made []string) (SweepCounts, error) {
	var c SweepCounts
	there, setup, err := PhoneAccess(ctx, kiffc)
	if err != nil {
		return c, err
	}
	for _, a := range made {
		switch {
		case a == setup:
			// Setup's agent is the account's, never a run's to take.
		case !there[a]:
			c.gone++
		default:
			ans, err := kiffc.Do(ctx, http.MethodPost, "/v1/me/agents/"+url.PathEscape(a)+"/archive", map[string]string{})
			switch {
			case err == nil && ans.Err() == nil:
				c.archived++
			case err == nil && ans.Status == http.StatusNotFound:
				// The prune deleted it between the read and the call.
				c.gone++
			default:
				c.failed++
			}
		}
	}
	// What's still there, as KIFF answers the phone after: an archived agent
	// has left its access.
	if there, _, err = PhoneAccess(ctx, kiffc); err == nil {
		for _, a := range made {
			if a != setup && there[a] {
				c.left++
			}
		}
	}
	if c.failed > 0 {
		return c, fmt.Errorf("%d of the sweep's calls failed", c.failed)
	}
	return c, err
}

// phoneAccess is the account's agents that aren't archived, as KIFF tells
// the phone (GET /v1/me/access), and which of them is Setup's.
func PhoneAccess(ctx context.Context, kiffc *Client) (there map[string]bool, setup string, err error) {
	a, err := kiffc.Do(ctx, http.MethodGet, "/v1/me/access", nil)
	if err != nil {
		return nil, "", err
	}
	if err := a.Err(); err != nil {
		return nil, "", fmt.Errorf("reading the phone's access: %w", err)
	}
	var out struct {
		Agents     map[string]json.RawMessage `json:"agents"`
		SetupAgent string                     `json:"setup_agent"`
	}
	if err := json.Unmarshal([]byte(a.Raw), &out); err != nil {
		return nil, "", fmt.Errorf("reading the phone's access: %w", err)
	}
	there = make(map[string]bool, len(out.Agents))
	for id := range out.Agents {
		there[id] = true
	}
	return there, out.SetupAgent, nil
}

// recordMade appends a taf this run made to -made, for the sweep after it.
func RecordMade(path, agent string) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(f, agent)
	return errors.Join(err, f.Close())
}

// readMade is the tafs this job's runs made; none when no run got that far.
func ReadMade(path string) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) || path == "" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return MadeFrom(f)
}

func MadeFrom(r io.Reader) ([]string, error) {
	var made []string
	s := bufio.NewScanner(r)
	for s.Scan() {
		if a := strings.TrimSpace(s.Text()); a != "" && !slices.Contains(made, a) {
			made = append(made, a)
		}
	}
	return made, s.Err()
}
