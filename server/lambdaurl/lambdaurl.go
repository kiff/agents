// Package lambdaurl serves an http.Handler behind a Lambda function URL whose
// auth type is AWS_IAM and invoke mode RESPONSE_STREAM: Lambda refuses any request not signed by a principal
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
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

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

// Serve runs h on the event for a function URL whose invoke mode is
// RESPONSE_STREAM: what h writes reaches the client as it writes it, so a
// server-sent event stream is live.
//
// h runs in its own goroutine, which Serve hands over to the returned
// response: it ends when h returns, or when the runtime closes the response
// (h's next write then fails). Serve returns once h has written or flushed
// its header, or has returned without one.
//
// A panic in h is logged to log (nil logs nothing) and ends the body with
// errFailed: the runtime sends a body's error to the client as the stream's
// error, so what the panic said stays here.
func Serve(ctx context.Context, h http.Handler, e events.LambdaFunctionURLRequest, log *slog.Logger) (*events.LambdaFunctionURLStreamingResponse, error) {
	r, err := Request(ctx, e)
	if err != nil {
		return &events.LambdaFunctionURLStreamingResponse{StatusCode: http.StatusBadRequest,
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    strings.NewReader(`{"error":"the request could not be read"}`)}, nil
	}
	pr, pw := io.Pipe()
	w := &streamWriter{header: http.Header{}, pipe: pw, ready: make(chan struct{})}
	go func() {
		defer func() {
			if p := recover(); p != nil {
				if log != nil {
					log.ErrorContext(ctx, "handler panicked", "path", r.URL.Path, "panic", fmt.Sprint(p))
				}
				w.WriteHeader(http.StatusInternalServerError) // if nothing was sent yet
				_ = pw.CloseWithError(errFailed)
				return
			}
			w.WriteHeader(http.StatusOK) // a handler that wrote nothing
			_ = pw.Close()
		}()
		h.ServeHTTP(w, r)
	}()
	<-w.ready
	out := &events.LambdaFunctionURLStreamingResponse{StatusCode: w.code, Headers: map[string]string{}, Body: pr}
	for k, v := range w.sent {
		if k == "Set-Cookie" {
			out.Cookies = append(out.Cookies, v...)
			continue
		}
		out.Headers[k] = strings.Join(v, ", ")
	}
	return out, nil
}

// errFailed ends a body whose handler failed. It is all the client learns:
// the runtime forwards a body's error to it.
var errFailed = errors.New("the response failed")

// streamWriter is an http.ResponseWriter whose body goes down a pipe. Its
// status and header are fixed when first written, as on any connection.
type streamWriter struct {
	header http.Header
	pipe   *io.PipeWriter

	once  sync.Once
	ready chan struct{} // closed once the status and header are fixed
	code  int
	sent  http.Header // the header as it was when fixed
}

func (w *streamWriter) Header() http.Header { return w.header }

func (w *streamWriter) WriteHeader(code int) {
	w.once.Do(func() {
		w.code, w.sent = code, w.header.Clone()
		close(w.ready)
	})
}

func (w *streamWriter) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.pipe.Write(b)
}

// Flush commits the status and header, as on any connection; the body needs
// nothing more, since every Write already goes down the pipe as it is made.
func (w *streamWriter) Flush() { w.WriteHeader(http.StatusOK) }

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
