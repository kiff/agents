package main

import (
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
	"strings"
	"time"
)

// client is a JSON client for one service, with a way to authenticate each
// request.
type client struct {
	base string
	auth func(req *http.Request, body []byte)
	http *http.Client
}

// answer is a JSON answer, with its status.
type answer struct {
	Status int
	Body   map[string]any
	Raw    string
}

func (a answer) err() error {
	if a.Status >= 200 && a.Status < 300 {
		return nil
	}
	msg, _ := a.Body["error"].(string)
	if m, _ := a.Body["message"].(string); m != "" {
		msg = m
	}
	return fmt.Errorf("%d: %s", a.Status, msg)
}

func (c *client) do(ctx context.Context, method, path string, body any) (answer, error) {
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			return answer{}, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(b))
	if err != nil {
		return answer{}, err
	}
	req.Header.Set("Accept", "application/json")
	if b != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.auth != nil {
		c.auth(req, b)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return answer{}, err
	}
	defer resp.Body.Close()
	// Read whole: an answer cut short decodes as nothing, and a grant that
	// was there reads as missing. An account's grants list grows with every
	// run (kiff/tafs#214).
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return answer{}, err
	}
	if len(raw) > maxAnswer {
		return answer{}, fmt.Errorf("%s %s: the answer is over %d bytes", method, path, maxAnswer)
	}
	a := answer{Status: resp.StatusCode, Raw: string(raw)}
	_ = json.Unmarshal(raw, &a.Body)
	return a, nil
}

// phone is the owner's phone toward planque: a P-256 key, as tafs'
// ApprovalSigner, signing each call (contract 2 on kiff/planque#1).
type phone struct {
	key     *ecdsa.PrivateKey
	planque *client
	tenant  string
	// device is this phone's id in KIFF, sent with each signature (d=), so
	// planque asks KIFF whether it signed, as the tafs app does
	// (kiff/planque#12). Empty signs with the key alone.
	device string
	// signIn completes the vendor's sign-in and returns the code from the
	// app's link: on its own at the fake vendor, by a person at Stripe.
	signIn func(ctx context.Context, link string) (string, error)
}

// parsePhone reads a base64 PKCS#8 P-256 key: the test's stable phone, so
// planque keeps it enrolled from one night to the next.
func parsePhone(b64 string) (*ecdsa.PrivateKey, error) {
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

func newPhone(key *ecdsa.PrivateKey, planqueURL, tenant string, hc *http.Client) *phone {
	p := &phone{key: key, tenant: tenant, signIn: signIn}
	base := strings.TrimRight(planqueURL, "/")
	p.planque = &client{base: base, http: hc, auth: func(req *http.Request, body []byte) {
		// To the nanosecond: planque refuses a signed request made twice, so
		// two reads in one second must differ.
		at := time.Now().UTC().Format(time.RFC3339Nano)
		sum := sha256.Sum256(body)
		msg := strings.Join([]string{"planque.request.v1", base, tenant, req.Method, req.URL.RequestURI(),
			hex.EncodeToString(sum[:]), at}, "\n")
		digest := sha256.Sum256([]byte(msg))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
		req.Header.Set("Planque-Device-Signature", signatureHeader(at, sig, p.device))
	}}
	return p
}

// signatureHeader is planque's device signature header: the time, the
// signature and, for a device KIFF knows, its id.
func signatureHeader(at string, sig []byte, device string) string {
	h := "t=" + at + ",v1=" + base64.StdEncoding.EncodeToString(sig)
	if device != "" {
		h += ",d=" + device
	}
	return h
}

// thisDevice is the phone's id in KIFF: the device its session signed in
// as (GET /v1/me/devices, this_device), as the tafs app reads it.
func thisDevice(ctx context.Context, kiff *client) (string, error) {
	a, err := kiff.do(ctx, http.MethodGet, "/v1/me/devices", nil)
	if err != nil {
		return "", err
	}
	if err := a.err(); err != nil {
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

func (p *phone) publicKey() string {
	der, _ := x509.MarshalPKIXPublicKey(&p.key.PublicKey)
	return base64.StdEncoding.EncodeToString(der)
}

func (p *phone) accountPath() string { return "/v1/accounts/" + url.PathEscape(p.tenant) }

// grantTool is one tool of a grant, as asked for.
type grantTool struct {
	Tool        string         `json:"tool"`
	Name        string         `json:"name"`
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
type grant struct {
	ID     string   `json:"id"`
	Status string   `json:"status"`
	Text   string   `json:"text"`
	Digest string   `json:"digest"`
	Tools  []string `json:"tools"`
	Reason string   `json:"reason"`
	// Request is what the owner approved: the tools, as asked for.
	Request struct {
		Tools []grantTool `json:"tools"`
	} `json:"request"`
	// Events is the grant's record, such as "read list_accounts at …" when
	// Setup read what the sign-in covers (kiff/planque#14).
	Events []struct {
		What string `json:"what"`
	} `json:"events"`
	Raw json.RawMessage `json:"-"`
}

// maxAnswer bounds an answer the e2e reads.
const maxAnswer = 32 << 20

func (p *phone) grants(ctx context.Context) ([]grant, error) {
	a, err := p.planque.do(ctx, http.MethodGet, p.accountPath()+"/grants", nil)
	if err != nil {
		return nil, err
	}
	if err := a.err(); err != nil {
		return nil, fmt.Errorf("planque grants: %w", err)
	}
	var out struct {
		Grants []grant `json:"grants"`
	}
	if err := json.Unmarshal([]byte(a.Raw), &out); err != nil {
		return nil, fmt.Errorf("planque grants: %w", err)
	}
	return out.Grants, nil
}

func (p *phone) grant(ctx context.Context, id string) (grant, error) {
	gs, err := p.grants(ctx)
	if err != nil {
		return grant{}, err
	}
	for _, g := range gs {
		if g.ID == id {
			return g, nil
		}
	}
	return grant{}, fmt.Errorf("planque has no grant %s for this account", id)
}

// approveAndSignIn is the owner approving a grant on the phone, signing in
// at the vendor in a browser that follows redirects up to the app's return
// link, and the phone confirming with the code that link carries. A grant
// whose sign-in planque already holds (its tools set after connecting
// first) connects on approval, with no sign-in. It returns the status.
func (p *phone) approveAndSignIn(ctx context.Context, g grant) (string, error) {
	a, err := p.planque.do(ctx, http.MethodPost, p.accountPath()+"/grants/"+url.PathEscape(g.ID)+"/approve", map[string]string{"digest": g.Digest})
	if err != nil {
		return "", err
	}
	if err := a.err(); err != nil {
		return "", fmt.Errorf("approve: %w", err)
	}
	if st, _ := a.Body["status"].(string); st == "connected" {
		return "connected, with no second sign-in", nil
	}
	link, _ := a.Body["sign_in_url"].(string)
	code, err := p.signIn(ctx, link)
	if err != nil {
		return "", err
	}
	a, err = p.planque.do(ctx, http.MethodPost, p.accountPath()+"/grants/"+url.PathEscape(g.ID)+"/confirm", map[string]string{"confirm": code})
	if err != nil {
		return "", err
	}
	if err := a.err(); err != nil {
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
func signIn(ctx context.Context, link string) (string, error) {
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

func (p *phone) disconnect(ctx context.Context, id string) error {
	a, err := p.planque.do(ctx, http.MethodDelete, p.accountPath()+"/grants/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return a.err()
}
