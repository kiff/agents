// Package lambdaurl serves an http.Handler behind a Lambda function URL whose
// auth type is AWS_IAM: Lambda refuses any request not signed by a principal
// allowed to invoke the URL, and tells the function who signed it.
//
// Auth trusts a request's tenant header only from the callers it is given
// (tafs cloud's role): they verify the person and their tenant before
// calling.
package lambdaurl

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/aws/aws-lambda-go/events"

	"github.com/kiff/agents/server"
)

// TenantHeader is the header a verified caller names the tenant in.
const TenantHeader = "Kiff-Tenant"

type callerKey struct{}

// callerARN is the ARN Lambda verified as a request's signer, or "".
func callerARN(r *http.Request) string {
	s, _ := r.Context().Value(callerKey{}).(string)
	return s
}

// Request is the event as an *http.Request, carrying the signer Lambda
// verified.
func Request(ctx context.Context, e events.LambdaFunctionURLRequest) (*http.Request, error) {
	body := []byte(e.Body)
	if e.IsBase64Encoded {
		b, err := base64.StdEncoding.DecodeString(e.Body)
		if err != nil {
			return nil, fmt.Errorf("lambdaurl: the body is not base64: %w", err)
		}
		body = b
	}
	if a := e.RequestContext.Authorizer; a != nil && a.IAM != nil {
		ctx = context.WithValue(ctx, callerKey{}, a.IAM.UserARN)
	}
	target := e.RawPath
	if e.RawQueryString != "" {
		target += "?" + e.RawQueryString
	}
	r, err := http.NewRequestWithContext(ctx, e.RequestContext.HTTP.Method, target, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("lambdaurl: %w", err)
	}
	for k, v := range e.Headers {
		r.Header.Set(k, v)
	}
	for _, c := range e.Cookies {
		r.Header.Add("Cookie", c)
	}
	r.RemoteAddr = e.RequestContext.HTTP.SourceIP
	r.RequestURI = target
	return r, nil
}

// Serve runs h on the event and returns its response.
func Serve(ctx context.Context, h http.Handler, e events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
	r, err := Request(ctx, e)
	if err != nil {
		return events.LambdaFunctionURLResponse{StatusCode: http.StatusBadRequest,
			Headers: map[string]string{"Content-Type": "application/json"}, Body: `{"error":"the request could not be read"}`}, nil
	}
	w := &response{header: http.Header{}}
	h.ServeHTTP(w, r)
	out := events.LambdaFunctionURLResponse{StatusCode: w.status(), Headers: map[string]string{}}
	for k, v := range w.header {
		if k == "Set-Cookie" {
			out.Cookies = append(out.Cookies, v...)
			continue
		}
		out.Headers[k] = strings.Join(v, ", ")
	}
	if utf8.Valid(w.body.Bytes()) {
		out.Body = w.body.String()
	} else {
		out.Body, out.IsBase64Encoded = base64.StdEncoding.EncodeToString(w.body.Bytes()), true
	}
	return out, nil
}

// response collects what a handler writes.
type response struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func (w *response) Header() http.Header { return w.header }

func (w *response) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}

func (w *response) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.body.Write(b)
}

func (w *response) status() int {
	if w.code == 0 {
		return http.StatusOK
	}
	return w.code
}

// Auth returns a server's Auth that accepts a request signed by one of
// roles, for the tenant in its TenantHeader. Each role is an assumed-role ARN
// prefix that ends at the role's name, such as
// "arn:aws:sts::<account>:assumed-role/<role>/": the session name after it
// varies by invocation.
func Auth(roles []string) (func(*http.Request) (server.Caller, error), error) {
	if len(roles) == 0 {
		return nil, errors.New("lambdaurl: no caller roles: nothing could call")
	}
	for _, r := range roles {
		if !strings.HasPrefix(r, "arn:aws:sts::") || !strings.Contains(r, ":assumed-role/") || !strings.HasSuffix(r, "/") ||
			strings.Count(r[strings.Index(r, ":assumed-role/")+len(":assumed-role/"):], "/") != 1 {
			return nil, fmt.Errorf("lambdaurl: %q is not an assumed-role ARN up to and including the role's name and its '/'", r)
		}
	}
	return func(req *http.Request) (server.Caller, error) {
		arn := callerARN(req)
		allowed := false
		for _, r := range roles {
			if strings.HasPrefix(arn, r) && len(arn) > len(r) {
				allowed = true
				break
			}
		}
		if !allowed {
			return server.Caller{}, errors.New("lambdaurl: not signed by a caller role")
		}
		tenant := req.Header.Get(TenantHeader)
		if tenant == "" {
			return server.Caller{}, errors.New("lambdaurl: no tenant header")
		}
		return server.Caller{Tenant: tenant}, nil
	}, nil
}
