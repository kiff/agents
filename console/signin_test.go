package console_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/console"
)

var key, otherKey = mustKey(), mustKey()

func mustKey() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}

// issuer is a fake OpenID provider: discovery, keys, and a token endpoint
// that checks the code, the client and the PKCE verifier, then answers with
// the ID token idToken makes for the nonce it was asked with.
type issuer struct {
	srv *httptest.Server
	// discovered overrides the discovery document's endpoints, by name.
	discovered map[string]string
	// jwksTo, when set, redirects the keys' request there.
	jwksTo    string
	challenge string // from the authorization request
	nonce     string
	idToken   func(nonce string) string
	exchanged int
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	is := &issuer{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]string{"issuer": is.srv.URL, "authorization_endpoint": is.srv.URL + "/authorize",
			"token_endpoint": is.srv.URL + "/token", "jwks_uri": is.srv.URL + "/jwks"}
		for k, v := range is.discovered {
			doc[k] = v
		}
		_ = json.NewEncoder(w).Encode(doc)
	})
	keys := func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "k1", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	}
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		if is.jwksTo != "" {
			http.Redirect(w, r, is.jwksTo, http.StatusFound)
			return
		}
		keys(w, r)
	})
	mux.HandleFunc("GET /keys", keys) // where a redirect may send the keys' request
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		is.exchanged++
		id, secret, _ := r.BasicAuth()
		sum := sha256.Sum256([]byte(r.FormValue("code_verifier")))
		if r.FormValue("code") != "code-1" || id != "console" || secret != "s3cret" ||
			base64.RawURLEncoding.EncodeToString(sum[:]) != is.challenge || r.FormValue("grant_type") != "authorization_code" {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": is.idToken(is.nonce), "token_type": "Bearer"})
	})
	is.srv = httptest.NewServer(mux)
	t.Cleanup(is.srv.Close)
	return is
}

// jwt signs claims with k, under key ID kid and algorithm alg.
func jwt(k *rsa.PrivateKey, alg, kid string, claims map[string]any) string {
	head, _ := json.Marshal(map[string]string{"alg": alg, "kid": kid, "typ": "JWT"})
	body, _ := json.Marshal(claims)
	signed := base64.RawURLEncoding.EncodeToString(head) + "." + base64.RawURLEncoding.EncodeToString(body)
	sum := sha256.Sum256([]byte(signed))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, sum[:])
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

var at = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

func (is *issuer) claims(sub, nonce string) map[string]any {
	return map[string]any{"iss": is.srv.URL, "sub": sub, "aud": "console", "exp": at.Add(5 * time.Minute).Unix(),
		"iat": at.Unix(), "nonce": nonce, "email": sub + "@example.test"}
}

type env struct {
	is    *issuer
	in    *console.SignIn
	clock *time.Time
	srv   *httptest.Server
}

func setup(t *testing.T) *env { t.Helper(); return setupWith(t, nil) }

// rerouted is a transport that sends every request to the issuer's server,
// whatever host it names: the plain-http hosts a redirect might name resolve.
type rerouted struct{ to *httptest.Server }

func (r rerouted) RoundTrip(req *http.Request) (*http.Response, error) {
	u, _ := url.Parse(r.to.URL)
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = u.Scheme, u.Host
	return r.to.Client().Transport.RoundTrip(req)
}

func setupWith(t *testing.T, change func(is *issuer, c *console.Config)) *env {
	t.Helper()
	is := newIssuer(t)
	e := &env{is: is, clock: new(time.Time)}
	*e.clock = at
	cfg := console.Config{Issuer: is.srv.URL, ClientID: "console", ClientSecret: "s3cret",
		BaseURL: "http://localhost:8080", Operators: []string{"user_op"}, CookieKey: []byte(strings.Repeat("k", 32)),
		HTTP: is.srv.Client(), Now: func() time.Time { return *e.clock }}
	if change != nil {
		change(is, &cfg)
	}
	in, err := console.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.in = in
	is.idToken = func(nonce string) string { return jwt(key, "RS256", "k1", is.claims("user_op", nonce)) }
	mux := http.NewServeMux()
	in.Routes(mux)
	mux.Handle("GET /console", in.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		op, _ := in.Operator(r)
		_, _ = w.Write([]byte("hello " + op.Subject))
	})))
	mux.Handle("POST /ops/thing", in.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

// noFollow is a client that shows redirects instead of following them.
func noFollow() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// login starts a sign-in: the issuer's authorization URL, and the sign-in
// cookie. It records the challenge and nonce the issuer was asked with.
func (e *env) login(t *testing.T) (*url.URL, *http.Cookie) {
	t.Helper()
	resp, err := noFollow().Get(e.srv.URL + "/console/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	q := loc.Query()
	e.is.challenge, e.is.nonce = q.Get("code_challenge"), q.Get("nonce")
	var c *http.Cookie
	for _, k := range resp.Cookies() {
		if k.Name == "console_signin" {
			c = k
		}
	}
	if c == nil {
		t.Fatal("no sign-in cookie")
	}
	return loc, c
}

// callback comes back from the issuer with query, and the sign-in cookie.
func (e *env) callback(t *testing.T, query string, c *http.Cookie) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+"/console/callback?"+query, nil)
	if c != nil {
		req.AddCookie(c)
	}
	resp, err := noFollow().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func session(resp *http.Response) *http.Cookie {
	for _, k := range resp.Cookies() {
		if k.Name == "console_session" && k.MaxAge > 0 {
			return k
		}
	}
	return nil
}

func (e *env) page(t *testing.T, c *http.Cookie) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+"/console", nil)
	if c != nil {
		req.AddCookie(c)
	}
	resp, err := noFollow().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// An operator signs in: the issuer is asked with PKCE, a state and a nonce;
// its code is exchanged with the verifier; the ID token is checked; the
// session lets the operator in until it ends.
func TestAnOperatorSignsIn(t *testing.T) {
	e := setup(t)
	loc, c := e.login(t)
	q := loc.Query()
	if loc.Path != "/authorize" || q.Get("client_id") != "console" || q.Get("response_type") != "code" ||
		q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != "http://localhost:8080/console/callback" ||
		!strings.Contains(q.Get("scope"), "openid") || q.Get("state") == "" || q.Get("nonce") == "" {
		t.Fatalf("authorization request %s", loc)
	}
	resp := e.callback(t, "code=code-1&state="+q.Get("state"), c)
	sess := session(resp)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/console" || sess == nil || !sess.HttpOnly {
		t.Fatalf("callback: %d to %q, session %+v", resp.StatusCode, resp.Header.Get("Location"), sess)
	}
	if code, body := e.page(t, sess); code != 200 || body != "hello user_op" {
		t.Fatalf("a page: %d %q", code, body)
	}
	*e.clock = at.Add(console.SessionTTL)
	if code, _ := e.page(t, sess); code != http.StatusSeeOther {
		t.Fatalf("an ended session: %d, want to sign in again", code)
	}
}

// Only an operator gets a session; anyone else who signs in is 403.
func TestSomeoneElseIsNotLetIn(t *testing.T) {
	e := setup(t)
	e.is.idToken = func(nonce string) string { return jwt(key, "RS256", "k1", e.is.claims("user_else", nonce)) }
	loc, c := e.login(t)
	resp := e.callback(t, "code=code-1&state="+loc.Query().Get("state"), c)
	if resp.StatusCode != http.StatusForbidden || session(resp) != nil {
		t.Fatalf("someone else: %d, session %v", resp.StatusCode, session(resp))
	}
}

// An ID token that doesn't check signs no one in, whatever is wrong with it.
func TestAnIDTokenThatDoesntCheck(t *testing.T) {
	for name, make := range map[string]func(is *issuer, nonce string) string{
		"another issuer": func(is *issuer, n string) string {
			c := is.claims("user_op", n)
			c["iss"] = "https://evil.test"
			return jwt(key, "RS256", "k1", c)
		},
		"another client": func(is *issuer, n string) string {
			c := is.claims("user_op", n)
			c["aud"] = "other"
			return jwt(key, "RS256", "k1", c)
		},
		"expired": func(is *issuer, n string) string {
			c := is.claims("user_op", n)
			c["exp"] = at.Add(-2 * time.Minute).Unix()
			return jwt(key, "RS256", "k1", c)
		},
		"from the future": func(is *issuer, n string) string {
			c := is.claims("user_op", n)
			c["iat"] = at.Add(5 * time.Minute).Unix()
			return jwt(key, "RS256", "k1", c)
		},
		"another nonce":   func(is *issuer, n string) string { return jwt(key, "RS256", "k1", is.claims("user_op", "replayed")) },
		"no subject":      func(is *issuer, n string) string { c := is.claims("", n); return jwt(key, "RS256", "k1", c) },
		"another key":     func(is *issuer, n string) string { return jwt(otherKey, "RS256", "k1", is.claims("user_op", n)) },
		"an unlisted key": func(is *issuer, n string) string { return jwt(key, "RS256", "k9", is.claims("user_op", n)) },
		"alg none": func(is *issuer, n string) string {
			t := jwt(key, "none", "k1", is.claims("user_op", n))
			return t[:strings.LastIndex(t, ".")+1]
		},
		"HS256": func(is *issuer, n string) string { return jwt(key, "HS256", "k1", is.claims("user_op", n)) },
		"a tampered claim": func(is *issuer, n string) string {
			return tamper(jwt(key, "RS256", "k1", is.claims("user_else", n)), "user_else", "user_op")
		},
		"not a JWT": func(is *issuer, n string) string { return "nope" },
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			e.is.idToken = func(nonce string) string { return make(e.is, nonce) }
			loc, c := e.login(t)
			resp := e.callback(t, "code=code-1&state="+loc.Query().Get("state"), c)
			if resp.StatusCode == http.StatusSeeOther || session(resp) != nil {
				t.Fatalf("signed in with %s: %d", name, resp.StatusCode)
			}
		})
	}
}

// tamper swaps from for to in a JWT's claims, keeping its signature.
func tamper(token, from, to string) string {
	parts := strings.Split(token, ".")
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	parts[1] = base64.RawURLEncoding.EncodeToString([]byte(strings.ReplaceAll(string(b), from, to)))
	return strings.Join(parts, ".")
}

// The callback checks the sign-in it ends: its state, its cookie, its age;
// a refusal from the issuer signs no one in; nor does a code exchanged with
// another verifier.
func TestACallbackThatDoesntCheck(t *testing.T) {
	e := setup(t)
	loc, c := e.login(t)
	state := loc.Query().Get("state")
	forged := *c
	forged.Value = forged.Value[:len(forged.Value)-2] + "xx"
	for name, cc := range map[string]struct {
		query  string
		cookie *http.Cookie
	}{
		"another state":   {"code=code-1&state=other", c},
		"no cookie":       {"code=code-1&state=" + state, nil},
		"a forged cookie": {"code=code-1&state=" + state, &forged},
		"refused":         {"error=access_denied&code=code-1&state=" + state, c},
		"no code":         {"state=" + state, c},
		"another code":    {"code=code-2&state=" + state, c},
	} {
		if resp := e.callback(t, cc.query, cc.cookie); resp.StatusCode != http.StatusUnauthorized || session(resp) != nil {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	e.is.challenge = "not the verifier's"
	if resp := e.callback(t, "code=code-1&state="+state, c); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a verifier that isn't the challenge's: %d", resp.StatusCode)
	}
	loc2, c2 := e.login(t)
	*e.clock = at.Add(11 * time.Minute)
	e.is.idToken = func(nonce string) string { // still current then
		cl := e.is.claims("user_op", nonce)
		cl["iat"], cl["exp"] = e.clock.Unix(), e.clock.Add(5*time.Minute).Unix()
		return jwt(key, "RS256", "k1", cl)
	}
	if resp := e.callback(t, "code=code-1&state="+loc2.Query().Get("state"), c2); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a sign-in that took too long: %d", resp.StatusCode)
	}
}

// A callback with another state leaves the sign-in in progress to finish:
// a link from elsewhere can't cancel it. The callback that ends it clears it.
func TestAStrayCallbackLeavesTheSignIn(t *testing.T) {
	e := setup(t)
	loc, c := e.login(t)
	for _, q := range []string{"code=code-1&state=junk", "error=access_denied&state=junk", "state="} {
		resp := e.callback(t, q, c)
		if resp.StatusCode != http.StatusUnauthorized || cleared(resp) {
			t.Fatalf("%s: %d, cleared %v", q, resp.StatusCode, cleared(resp))
		}
	}
	resp := e.callback(t, "code=code-1&state="+loc.Query().Get("state"), c)
	if resp.StatusCode != http.StatusSeeOther || session(resp) == nil || !cleared(resp) {
		t.Fatalf("the sign-in's own callback: %d, cleared %v", resp.StatusCode, cleared(resp))
	}
}

// cleared reports whether resp clears the sign-in cookie.
func cleared(resp *http.Response) bool {
	for _, k := range resp.Cookies() {
		if k.Name == "console_signin" && k.MaxAge < 0 {
			return true
		}
	}
	return false
}

// A cookie opens only as what it was sealed as, and a sign-in names its
// state, nonce and verifier. Here the issuer would take an empty verifier
// and nonce, so only these checks stop each sign-in.
func TestACookieIsOnlyWhatItWasSealedAs(t *testing.T) {
	e := setup(t)
	loc, c := e.login(t)
	sess := session(e.callback(t, "code=code-1&state="+loc.Query().Get("state"), c))
	if sess == nil {
		t.Fatal("no session")
	}
	accept := func(verifier, nonce string) {
		sum := sha256.Sum256([]byte(verifier))
		e.is.challenge, e.is.nonce = base64.RawURLEncoding.EncodeToString(sum[:]), nonce
	}
	refused := func(name, query string, value string) {
		t.Helper()
		k := &http.Cookie{Name: "console_signin", Value: value}
		if resp := e.callback(t, query, k); resp.StatusCode != http.StatusUnauthorized || session(resp) != nil {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	exp := at.Add(5 * time.Minute).Unix()

	accept("", "")
	refused("a session as a sign-in", "code=code-1&state=", sess.Value)

	empty, _ := e.in.Seal("console_signin", map[string]any{"exp": exp})
	refused("a sign-in with no state, nonce or verifier", "code=code-1&state=", empty)

	accept("v", "n")
	other, _ := e.in.Seal("console_session", map[string]any{"state": "s", "nonce": "n", "verifier": "v", "exp": exp})
	refused("a sign-in sealed as a session", "code=code-1&state=s", other)
	good, _ := e.in.Seal("console_signin", map[string]any{"state": "s", "nonce": "n", "verifier": "v", "exp": exp})
	if resp := e.callback(t, "code=code-1&state=s", &http.Cookie{Name: "console_signin", Value: good}); session(resp) == nil {
		t.Fatalf("the same sign-in sealed as one: %d", resp.StatusCode)
	}

	asSession := &http.Cookie{Name: "console_session", Value: c.Value}
	if code, _ := e.page(t, asSession); code != http.StatusSeeOther {
		t.Errorf("a sign-in as a session: %d", code)
	}
}

// A session is only good as sealed, for an operator still on the list;
// signing out ends it; without one, a page sends you to sign in and anything
// else is 401.
func TestSessions(t *testing.T) {
	e := setup(t)
	loc, c := e.login(t)
	sess := session(e.callback(t, "code=code-1&state="+loc.Query().Get("state"), c))
	forged := *sess
	payload, _, _ := strings.Cut(forged.Value, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	other := base64.RawURLEncoding.EncodeToString([]byte(strings.ReplaceAll(string(raw), "user_op", "user_xx")))
	forged.Value = other + forged.Value[len(payload):]
	if code, _ := e.page(t, &forged); code != http.StatusSeeOther {
		t.Fatalf("a forged session: %d", code)
	}
	if code, _ := e.page(t, nil); code != http.StatusSeeOther {
		t.Fatalf("no session: %d", code)
	}
	req, _ := http.NewRequest("POST", e.srv.URL+"/ops/thing", nil)
	if resp, _ := noFollow().Do(req); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a POST with no session: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("POST", e.srv.URL+"/console/logout", nil)
	resp, _ := noFollow().Do(req)
	for _, k := range resp.Cookies() {
		if k.Name == "console_session" && k.MaxAge >= 0 {
			t.Fatalf("logout kept the session: %+v", k)
		}
	}

	// An operator removed from the list is out, whatever their session says.
	in, _ := console.New(console.Config{Issuer: e.is.srv.URL, ClientID: "console", BaseURL: "http://localhost:8080",
		Operators: []string{"someone_else"}, CookieKey: []byte(strings.Repeat("k", 32)), Now: func() time.Time { return at }})
	r := httptest.NewRequest("GET", "/console", nil)
	r.AddCookie(sess)
	if _, err := in.Operator(r); err == nil {
		t.Fatal("a removed operator's session still checks")
	}
}

func TestConfigNeeds(t *testing.T) {
	ok := console.Config{Issuer: "https://id.example", ClientID: "c", BaseURL: "https://console.example",
		Operators: []string{"u"}, CookieKey: []byte(strings.Repeat("k", 32))}
	if _, err := console.New(ok); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*console.Config){
		"no issuer":           func(c *console.Config) { c.Issuer = "" },
		"no client":           func(c *console.Config) { c.ClientID = "" },
		"a short key":         func(c *console.Config) { c.CookieKey = []byte("short") },
		"no operators":        func(c *console.Config) { c.Operators = nil },
		"plain http":          func(c *console.Config) { c.BaseURL = "http://console.example" },
		"an empty operator":   func(c *console.Config) { c.Operators = []string{"u", ""} },
		"a plain-http issuer": func(c *console.Config) { c.Issuer = "http://id.example" },
		"a relative issuer":   func(c *console.Config) { c.Issuer = "id.example" },
	} {
		c := ok
		change(&c)
		if _, err := console.New(c); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// The issuer's keys are trusted only at 2048 bits or more: a token signed
// with a short key the issuer lists signs no one in.
func TestAShortKeyIsNotTrusted(t *testing.T) {
	short, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	is := &issuer{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": is.srv.URL, "authorization_endpoint": is.srv.URL + "/authorize",
			"token_endpoint": is.srv.URL + "/token", "jwks_uri": is.srv.URL + "/jwks"})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "k1",
			"n": base64.RawURLEncoding.EncodeToString(short.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(short.E)).Bytes())}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": jwt(short, "RS256", "k1", is.claims("user_op", is.nonce))})
	})
	is.srv = httptest.NewServer(mux)
	defer is.srv.Close()
	in, err := console.New(console.Config{Issuer: is.srv.URL, ClientID: "console", BaseURL: "http://localhost:8080",
		Operators: []string{"user_op"}, CookieKey: []byte(strings.Repeat("k", 32)), HTTP: is.srv.Client(), Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	app := http.NewServeMux()
	in.Routes(app)
	srv := httptest.NewServer(app)
	defer srv.Close()
	e := &env{is: is, in: in, srv: srv, clock: new(time.Time)}
	loc, c := e.login(t)
	if resp := e.callback(t, "code=code-1&state="+loc.Query().Get("state"), c); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a 1024-bit key: %d", resp.StatusCode)
	}
}

// The issuer is reached only over https (loopback aside): a discovery
// document naming an endpoint that isn't, or a redirect to plain http, signs
// no one in, and nothing is sent there.
func TestTheIssuerIsReachedOnlyOverHTTPS(t *testing.T) {
	for name, set := range map[string]func(is *issuer){
		"an http token endpoint": func(is *issuer) { is.discovered = map[string]string{"token_endpoint": "http://id.example/token"} },
		"an http keys endpoint":  func(is *issuer) { is.discovered = map[string]string{"jwks_uri": "http://id.example/jwks"} },
		"an http authorization endpoint": func(is *issuer) {
			is.discovered = map[string]string{"authorization_endpoint": "http://id.example/authorize"}
		},
		"a relative endpoint":     func(is *issuer) { is.discovered = map[string]string{"token_endpoint": "/token"} },
		"keys redirected to http": func(is *issuer) { is.jwksTo = "http://id.example/jwks" },
	} {
		t.Run(name, func(t *testing.T) {
			// Every host the issuer names resolves to it, so only the
			// https rule can stop the sign-in.
			e := setupWith(t, func(is *issuer, c *console.Config) { c.HTTP = &http.Client{Transport: rerouted{is.srv}} })
			set(e.is)
			resp, err := noFollow().Get(e.srv.URL + "/console/login")
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if name == "keys redirected to http" { // discovery is fine: the sign-in fails at the keys
				loc, _ := url.Parse(resp.Header.Get("Location"))
				e.is.challenge, e.is.nonce = loc.Query().Get("code_challenge"), loc.Query().Get("nonce")
				var c *http.Cookie
				for _, k := range resp.Cookies() {
					if k.Name == "console_signin" {
						c = k
					}
				}
				if r := e.callback(t, "code=code-1&state="+loc.Query().Get("state"), c); r.StatusCode != http.StatusUnauthorized || session(r) != nil {
					t.Fatalf("callback: %d", r.StatusCode)
				}
				return
			}
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("login: %d, want the sign-in refused before anything is sent", resp.StatusCode)
			}
		})
	}
}
