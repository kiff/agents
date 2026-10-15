package lambdaurl_test

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"

	"github.com/kiff/agents/server/lambdaurl"
)

var ctx = context.Background()

// A made-up account: no real one is ever written here.
const role = "arn:aws:sts::000000000000:assumed-role/tafs-cloud/"

func event(method, path, query string, headers map[string]string, signer string) events.LambdaFunctionURLRequest {
	e := events.LambdaFunctionURLRequest{RawPath: path, RawQueryString: query, Headers: headers,
		RequestContext: events.LambdaFunctionURLRequestContext{HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: method, SourceIP: "198.51.100.7"}}}
	if signer != "" {
		e.RequestContext.Authorizer = &events.LambdaFunctionURLRequestContextAuthorizerDescription{
			IAM: &events.LambdaFunctionURLRequestContextAuthorizerIAMDescription{UserARN: signer}}
	}
	return e
}

func TestRequestCarriesTheEvent(t *testing.T) {
	e := event("PUT", "/agents/payouts", "a=1&b=2", map[string]string{"content-type": "application/json", "kiff-tenant": "t1"}, role+"tafs-cloud")
	e.Body, e.IsBase64Encoded = base64.StdEncoding.EncodeToString([]byte(`{"x":1}`)), true
	e.Cookies = []string{"c=1"}
	r, err := lambdaurl.Request(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r.Body)
	if r.Method != "PUT" || r.URL.Path != "/agents/payouts" || r.URL.Query().Get("b") != "2" || string(body) != `{"x":1}` ||
		r.Header.Get("Content-Type") != "application/json" || r.Header.Get(lambdaurl.TenantHeader) != "t1" ||
		r.Header.Get("Cookie") != "c=1" || r.RemoteAddr != "198.51.100.7" {
		t.Fatalf("got %s %s %q body %q headers %v from %s", r.Method, r.URL.Path, r.URL.RawQuery, body, r.Header, r.RemoteAddr)
	}
	e.Body = "not base64!"
	if _, err := lambdaurl.Request(ctx, e); err == nil {
		t.Fatal("accepted a body that isn't base64")
	}
}

func TestServeReturnsTheResponse(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Add("Set-Cookie", "a=1")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"path":"` + r.URL.Path + `"}`))
	})
	out, err := lambdaurl.Serve(ctx, h, event("POST", "/schedules", "", nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	if out.StatusCode != 201 || out.Body != `{"path":"/schedules"}` || out.IsBase64Encoded ||
		out.Headers["Content-Type"] != "application/json" || len(out.Cookies) != 1 || out.Cookies[0] != "a=1" {
		t.Fatalf("got %+v", out)
	}

	binary := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte{0xff, 0x00}) })
	out, _ = lambdaurl.Serve(ctx, binary, event("GET", "/", "", nil, ""))
	if out.StatusCode != 200 || !out.IsBase64Encoded || out.Body != base64.StdEncoding.EncodeToString([]byte{0xff, 0x00}) {
		t.Fatalf("binary body: %+v", out)
	}

	bad := event("GET", "/", "", nil, "")
	bad.Body, bad.IsBase64Encoded = "%%%", true
	out, err = lambdaurl.Serve(ctx, h, bad)
	if err != nil || out.StatusCode != 400 {
		t.Fatalf("an unreadable request: %+v, %v", out, err)
	}
}

func TestAuthTrustsTheTenantOnlyFromACallerRole(t *testing.T) {
	auth, err := lambdaurl.Auth([]string{role})
	if err != nil {
		t.Fatal(err)
	}
	caller := func(signer, tenant string) (string, error) {
		h := map[string]string{}
		if tenant != "" {
			h["kiff-tenant"] = tenant
		}
		r, err := lambdaurl.Request(ctx, event("GET", "/agents", "", h, signer))
		if err != nil {
			t.Fatal(err)
		}
		c, err := auth(r)
		return c.Tenant, err
	}
	if tenant, err := caller(role+"tafs-cloud-fn", "t1"); err != nil || tenant != "t1" {
		t.Fatalf("tafs cloud's role: %q, %v", tenant, err)
	}
	for name, signer := range map[string]string{
		"no signer (an unauthenticated URL)": "",
		"another role":                       "arn:aws:sts::000000000000:assumed-role/someone-else/s",
		"a role whose name starts the same":  "arn:aws:sts::000000000000:assumed-role/tafs-cloud-evil/s",
		"the role in another account":        "arn:aws:sts::999999999999:assumed-role/tafs-cloud/s",
		"the role with no session":           role,
		"a user":                             "arn:aws:iam::000000000000:user/tafs-cloud",
	} {
		if tenant, err := caller(signer, "t1"); err == nil {
			t.Errorf("%s: accepted as %q", name, tenant)
		}
	}
	if _, err := caller(role+"tafs-cloud-fn", ""); err == nil {
		t.Error("accepted a request with no tenant")
	}
}

func TestAuthRefusesARoleThatCouldMatchOthers(t *testing.T) {
	for _, r := range []string{
		"",
		"arn:aws:sts::000000000000:assumed-role/tafs-cloud",   // would match tafs-cloud-evil
		"arn:aws:sts::000000000000:assumed-role/",             // any role
		"arn:aws:sts::000000000000:assumed-role/tafs-cloud/s", // one session only, and not a prefix
		"arn:aws:iam::000000000000:role/tafs-cloud/",          // not what Lambda reports
		"tafs-cloud/",
	} {
		if _, err := lambdaurl.Auth([]string{r}); err == nil {
			t.Errorf("%q: accepted", r)
		}
	}
	if _, err := lambdaurl.Auth(nil); err == nil {
		t.Error("no roles: accepted")
	}
	if !strings.HasSuffix(role, "/") {
		t.Fatal("the test role must end at its name")
	}
}
