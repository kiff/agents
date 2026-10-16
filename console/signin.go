// Package console is the operators' console: what the agents do, read-only,
// across tenants. Operators sign in with KIFF's sign-in (OpenID Connect
// against KIFF's identity provider), and only the operators named in Config
// get in.
package console

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Session and sign-in limits.
const (
	// SessionTTL is how long an operator stays signed in.
	SessionTTL = 8 * time.Hour
	// signInTTL is how long a sign-in may take, from the redirect to the
	// callback.
	signInTTL = 10 * time.Minute
	// keysTTL is how long the issuer's signing keys are trusted before
	// they're read again; an unknown key ID reads them at once.
	keysTTL = time.Hour
	// maxResponse bounds what the issuer's endpoints return.
	maxResponse = 1 << 20
	// skew is the clock difference allowed on a token's times.
	skew = time.Minute
)

// Cookie names.
const (
	sessionCookie = "console_session"
	signInCookie  = "console_signin"
)

// ErrNotOperator means the person signed in, but isn't an operator.
var ErrNotOperator = errors.New("console: not an operator")

// Config is the console's sign-in.
type Config struct {
	// Issuer is KIFF's identity provider, as its ID tokens name it. Its
	// discovery document is at Issuer + "/.well-known/openid-configuration".
	Issuer string
	// ClientID and ClientSecret are the console's, registered with it.
	ClientID, ClientSecret string
	// BaseURL is where the console is served, e.g. https://console.example
	// (no trailing slash): the callback is BaseURL + "/console/callback".
	BaseURL string
	// Operators are the subjects ("sub") allowed in.
	Operators []string
	// CookieKey signs the console's cookies: 32 random bytes or more.
	CookieKey []byte
	// HTTP talks to the issuer; nil is http.DefaultClient.
	HTTP *http.Client
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Operator is who is signed in.
type Operator struct {
	Subject string `json:"sub"`
	Email   string `json:"email,omitempty"`
}

// SignIn is the console's sign-in: its routes, and the check every page makes.
type SignIn struct {
	cfg Config

	mu        sync.Mutex
	discovery *discovery
	keys      map[string]*rsa.PublicKey
	keysRead  time.Time
}

// New checks cfg and returns the sign-in.
func New(cfg Config) (*SignIn, error) {
	switch {
	case cfg.Issuer == "" || cfg.ClientID == "" || cfg.BaseURL == "":
		return nil, errors.New("console: the issuer, the client ID and the base URL are required")
	case len(cfg.CookieKey) < 32:
		return nil, errors.New("console: the cookie key is 32 bytes or more")
	case len(cfg.Operators) == 0 || slices.Contains(cfg.Operators, ""):
		return nil, errors.New("console: name at least one operator, and no empty one")
	}
	if !secureURL(cfg.BaseURL) {
		return nil, errors.New("console: the base URL is https (or loopback, for local use)")
	}
	if !secureURL(cfg.Issuer) {
		return nil, errors.New("console: the issuer is https (or loopback, for local use)")
	}
	cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")
	return &SignIn{cfg: cfg}, nil
}

// secureURL reports whether raw is an absolute https URL, or http on a
// loopback host (for local use and tests). The sign-in sends the client's
// secret and trusts signing keys only over these (OIDC Discovery 3).
func secureURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		h := u.Hostname()
		return h == "localhost" || h == "127.0.0.1" || h == "::1"
	}
	return false
}

func (s *SignIn) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

// http is the client the issuer is reached with: it never follows a
// redirect to a URL that isn't secure.
func (s *SignIn) http() *http.Client {
	c := http.Client{}
	if s.cfg.HTTP != nil {
		c = *s.cfg.HTTP
	}
	next := c.CheckRedirect
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !secureURL(req.URL.String()) {
			return errors.New("console: the issuer redirected to a URL that isn't https")
		}
		if next != nil {
			return next(req, via)
		}
		if len(via) >= 10 {
			return errors.New("console: too many redirects")
		}
		return nil
	}
	return &c
}

func (s *SignIn) secure() bool { return strings.HasPrefix(s.cfg.BaseURL, "https://") }

// Routes adds /console/login, /console/callback and /console/logout to mux.
func (s *SignIn) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /console/login", s.login)
	mux.HandleFunc("GET /console/callback", s.callback)
	mux.HandleFunc("POST /console/logout", s.logout)
}

// Operator is the operator r's session names, or an error: no session, or
// one that doesn't check or has ended.
func (s *SignIn) Operator(r *http.Request) (Operator, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return Operator{}, errors.New("console: not signed in")
	}
	var sess struct {
		Operator
		Expires int64 `json:"exp"`
	}
	if err := s.open(sessionCookie, c.Value, &sess); err != nil {
		return Operator{}, err
	}
	if s.now().Unix() >= sess.Expires {
		return Operator{}, errors.New("console: the session has ended")
	}
	if !slices.Contains(s.cfg.Operators, sess.Subject) {
		return Operator{}, ErrNotOperator // removed from the operators since
	}
	return sess.Operator, nil
}

// Require serves next only to a signed-in operator; anyone else is sent to
// sign in (a page) or answered 401 (anything else).
func (s *SignIn) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.Operator(r); err != nil {
			if r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/ops/") {
				http.Redirect(w, r, "/console/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "sign in as an operator", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// login sends the operator to the issuer, with a fresh state, nonce and
// PKCE verifier kept in a short-lived signed cookie.
func (s *SignIn) login(w http.ResponseWriter, r *http.Request) {
	d, err := s.discover(r.Context())
	if err != nil {
		http.Error(w, "sign-in is unavailable", http.StatusBadGateway)
		return
	}
	st := signIn{State: random(), Nonce: random(), Verifier: random(), Expires: s.now().Add(signInTTL).Unix()}
	sealed, err := s.seal(signInCookie, st)
	if err != nil {
		http.Error(w, "sign-in is unavailable", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: signInCookie, Value: sealed, Path: "/console/callback", MaxAge: int(signInTTL.Seconds()),
		HttpOnly: true, Secure: s.secure(), SameSite: http.SameSiteLaxMode})
	challenge := sha256.Sum256([]byte(st.Verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {s.cfg.ClientID},
		"redirect_uri":          {s.cfg.BaseURL + "/console/callback"},
		"scope":                 {"openid email"},
		"state":                 {st.State},
		"nonce":                 {st.Nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	http.Redirect(w, r, d.Authorization+"?"+q.Encode(), http.StatusFound)
}

// signIn is what a sign-in in progress keeps, in its cookie.
type signIn struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	Expires  int64  `json:"exp"`
}

// callback takes the issuer's code, checks its ID token, and signs the
// operator in.
func (s *SignIn) callback(w http.ResponseWriter, r *http.Request) {
	refuse := func(why string) { http.Error(w, why, http.StatusUnauthorized) }
	c, err := r.Cookie(signInCookie)
	if err != nil {
		refuse("no sign-in in progress")
		return
	}
	var st signIn
	if err := s.open(signInCookie, c.Value, &st); err != nil || s.now().Unix() >= st.Expires ||
		st.State == "" || st.Nonce == "" || st.Verifier == "" {
		refuse("the sign-in doesn't check, or took too long")
		return
	}
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1 {
		refuse("the sign-in doesn't check")
		return
	}
	// Only the callback for this sign-in ends it: one with another state
	// (a link from elsewhere, say) leaves it to finish.
	http.SetCookie(w, &http.Cookie{Name: signInCookie, Value: "", Path: "/console/callback", MaxAge: -1,
		HttpOnly: true, Secure: s.secure(), SameSite: http.SameSiteLaxMode})
	if q.Get("error") != "" {
		refuse("the sign-in was refused")
		return
	}
	if q.Get("code") == "" {
		refuse("the sign-in doesn't check")
		return
	}
	op, err := s.exchange(r.Context(), q.Get("code"), st)
	switch {
	case errors.Is(err, ErrNotOperator):
		http.Error(w, "this account isn't an operator's", http.StatusForbidden)
		return
	case err != nil:
		refuse("the sign-in doesn't check")
		return
	}
	sealed, err := s.seal(sessionCookie, struct {
		Operator
		Expires int64 `json:"exp"`
	}{op, s.now().Add(SessionTTL).Unix()})
	if err != nil {
		http.Error(w, "sign-in is unavailable", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: sealed, Path: "/", MaxAge: int(SessionTTL.Seconds()),
		HttpOnly: true, Secure: s.secure(), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/console", http.StatusSeeOther)
}

// logout ends the session. It's a POST, so a link elsewhere can't sign an
// operator out.
func (s *SignIn) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure(), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/console/login", http.StatusSeeOther)
}

// exchange trades the code for tokens and checks the ID token: signed by
// the issuer, for this client, current, with this sign-in's nonce, and an
// operator's.
func (s *SignIn) exchange(ctx context.Context, code string, st signIn) (Operator, error) {
	d, err := s.discover(ctx)
	if err != nil {
		return Operator{}, err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {s.cfg.BaseURL + "/console/callback"},
		"client_id":     {s.cfg.ClientID},
		"code_verifier": {st.Verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return Operator{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if s.cfg.ClientSecret != "" {
		req.SetBasicAuth(url.QueryEscape(s.cfg.ClientID), url.QueryEscape(s.cfg.ClientSecret))
	}
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if err := s.do(req, &tok); err != nil {
		return Operator{}, fmt.Errorf("console: the token exchange: %w", err)
	}
	claims, err := s.verify(ctx, tok.IDToken)
	if err != nil {
		return Operator{}, err
	}
	if subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(st.Nonce)) != 1 {
		return Operator{}, errors.New("console: the ID token's nonce isn't this sign-in's")
	}
	if !slices.Contains(s.cfg.Operators, claims.Subject) {
		return Operator{}, ErrNotOperator
	}
	return Operator{Subject: claims.Subject, Email: claims.Email}, nil
}

// claims is what the console reads of an ID token.
type claims struct {
	Issuer   string   `json:"iss"`
	Subject  string   `json:"sub"`
	Audience audience `json:"aud"`
	Expires  int64    `json:"exp"`
	IssuedAt int64    `json:"iat"`
	Nonce    string   `json:"nonce"`
	Email    string   `json:"email"`
}

// audience is "aud": one string or several.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

// verify checks an ID token's RS256 signature against the issuer's keys and
// its claims against this client and the clock.
func (s *SignIn) verify(ctx context.Context, token string) (claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims{}, errors.New("console: not a JWT")
	}
	var head struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodePart(parts[0], &head); err != nil {
		return claims{}, err
	}
	if head.Alg != "RS256" {
		return claims{}, fmt.Errorf("console: the ID token is signed with %q, not RS256", head.Alg)
	}
	key, err := s.key(ctx, head.Kid)
	if err != nil {
		return claims{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return claims{}, errors.New("console: the ID token's signature isn't base64url")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return claims{}, errors.New("console: the ID token's signature doesn't check")
	}
	var c claims
	if err := decodePart(parts[1], &c); err != nil {
		return claims{}, err
	}
	now := s.now()
	switch {
	case c.Issuer != s.cfg.Issuer:
		return claims{}, errors.New("console: the ID token is another issuer's")
	case !slices.Contains(c.Audience, s.cfg.ClientID):
		return claims{}, errors.New("console: the ID token is for another client")
	case c.Subject == "":
		return claims{}, errors.New("console: the ID token names no subject")
	case now.After(time.Unix(c.Expires, 0).Add(skew)):
		return claims{}, errors.New("console: the ID token has expired")
	case time.Unix(c.IssuedAt, 0).After(now.Add(skew)):
		return claims{}, errors.New("console: the ID token is from the future")
	}
	return c, nil
}

func decodePart(part string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return errors.New("console: a JWT part isn't base64url")
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("console: a JWT part isn't JSON: %w", err)
	}
	return nil
}

// discovery is what the console reads of the issuer's discovery document.
type discovery struct {
	Issuer        string `json:"issuer"`
	Authorization string `json:"authorization_endpoint"`
	Token         string `json:"token_endpoint"`
	JWKS          string `json:"jwks_uri"`
}

func (s *SignIn) discover(ctx context.Context) (*discovery, error) {
	s.mu.Lock()
	d := s.discovery
	s.mu.Unlock()
	if d != nil {
		return d, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(s.cfg.Issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	d = new(discovery)
	if err := s.do(req, d); err != nil {
		return nil, fmt.Errorf("console: the issuer's discovery: %w", err)
	}
	if d.Issuer != s.cfg.Issuer || !secureURL(d.Authorization) || !secureURL(d.Token) || !secureURL(d.JWKS) {
		return nil, errors.New("console: the issuer's discovery document doesn't match it, or names an endpoint that isn't https")
	}
	s.mu.Lock()
	s.discovery = d
	s.mu.Unlock()
	return d, nil
}

// key is the issuer's RSA signing key with ID kid, read again when it's
// unknown or the keys are old.
func (s *SignIn) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	s.mu.Lock()
	k, ok := s.keys[kid]
	fresh := s.now().Sub(s.keysRead) < keysTTL
	s.mu.Unlock()
	if ok && fresh {
		return k, nil
	}
	d, err := s.discover(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.JWKS, nil)
	if err != nil {
		return nil, err
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := s.do(req, &set); err != nil {
		return nil, fmt.Errorf("console: the issuer's keys: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, jk := range set.Keys {
		if jk.Kty != "RSA" || (jk.Use != "" && jk.Use != "sig") {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(jk.N)
		e, err2 := base64.RawURLEncoding.DecodeString(jk.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if pub.N.BitLen() < 2048 {
			continue
		}
		keys[jk.Kid] = pub
	}
	s.mu.Lock()
	s.keys, s.keysRead = keys, s.now()
	s.mu.Unlock()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("console: the ID token is signed with a key the issuer doesn't list")
}

// do sends req and decodes a 200's JSON body into v.
func (s *SignIn) do(req *http.Request, v any) error {
	req.Header.Set("Accept", "application/json")
	resp, err := s.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", req.URL.Host, resp.StatusCode)
	}
	return json.Unmarshal(body, v)
}

// seal is v as JSON, signed with the cookie key for the cookie named
// purpose: payload.mac, base64url. One cookie's value doesn't open as
// another's.
func (s *SignIn) seal(purpose string, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(b)
	return payload + "." + base64.RawURLEncoding.EncodeToString(s.mac(purpose, payload)), nil
}

// open checks a sealed value's signature, for the cookie named purpose, and
// decodes it into v.
func (s *SignIn) open(purpose, sealed string, v any) error {
	payload, sig, ok := strings.Cut(sealed, ".")
	if !ok {
		return errors.New("console: the cookie doesn't check")
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.mac(purpose, payload)) {
		return errors.New("console: the cookie doesn't check")
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return errors.New("console: the cookie doesn't check")
	}
	return json.NewDecoder(bytes.NewReader(b)).Decode(v)
}

func (s *SignIn) mac(purpose, payload string) []byte {
	m := hmac.New(sha256.New, s.cfg.CookieKey)
	m.Write([]byte("console|" + purpose + "|" + payload))
	return m.Sum(nil)
}

// random is 32 random bytes, base64url.
func random() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
