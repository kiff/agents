package seed

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// A call whose connection is reset under it is tried once more, with the same
// idempotency key, so Stripe makes what it asked for once (Eval 44005248914:
// the hand-offs seed died on a GOAWAY). A refusal is never tried again.
func TestADroppedConnectionIsTriedOnceWithTheSameKey(t *testing.T) {
	var (
		mu    sync.Mutex
		keys  []string
		calls int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		mu.Unlock()
		if n == 1 {
			// The first time, the connection is reset under the call.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.(*net.TCPConn).SetLinger(0)
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte(`{"id":"dp_1"}`))
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "sk_test_x", nil)
	if err != nil {
		t.Fatal(err)
	}
	var out object
	if err := c.do(context.Background(), http.MethodPost, "/v1/disputes/dp_1", url.Values{"a": {"1"}}, &out); err != nil {
		t.Fatalf("a reset call isn't tried again: %v", err)
	}
	if out.ID != "dp_1" || calls != 2 {
		t.Fatalf("got %q after %d calls, want dp_1 after 2", out.ID, calls)
	}
	if keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("the retry's idempotency key %q isn't the first's %q", keys[1], keys[0])
	}
}

// A GET whose answer is cut off before its end is tried once more (Eval
// 44023858161: the hand-offs seed died on "unexpected EOF" listing disputes).
func TestAnAnswerCutOffIsTriedOnce(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			// The answer says 100 bytes and the connection ends after a few.
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte(`{"data":`))
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte(`{"id":"dp_1"}`))
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "sk_test_x", nil)
	if err != nil {
		t.Fatal(err)
	}
	var out object
	if err := c.do(context.Background(), http.MethodGet, "/v1/disputes", url.Values{"limit": {"100"}}, &out); err != nil {
		t.Fatalf("a cut-off answer isn't tried again: %v", err)
	}
	if out.ID != "dp_1" || calls != 2 {
		t.Fatalf("got %q after %d calls, want dp_1 after 2", out.ID, calls)
	}
}

func TestARefusalIsNotTriedAgain(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"no"}}`))
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "sk_test_x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.do(context.Background(), http.MethodPost, "/v1/disputes/dp_1", nil, nil); err == nil || calls != 1 {
		t.Fatalf("a refusal: err %v after %d calls, want an error after 1", err, calls)
	}
}

func TestOnlyADroppedConnectionCounts(t *testing.T) {
	for _, tc := range []struct {
		err  string
		want bool
	}{
		{`Post "x": http2: server sent GOAWAY and closed the connection; LastStreamID=1999, ErrCode=NO_ERROR, debug=""`, true},
		{"context deadline exceeded", false},
		{"seed: /v1/disputes: 400 no", false},
	} {
		if got := dropped(errString(tc.err)); got != tc.want {
			t.Errorf("dropped(%q) = %v, want %v", tc.err, got, tc.want)
		}
	}
	// An answer cut off, as net/http says it: the error wraps the EOF.
	for _, cut := range []error{io.ErrUnexpectedEOF, io.EOF} {
		if err := (&url.Error{Op: "Get", URL: "https://api.stripe.com/v1/disputes", Err: cut}); !dropped(err) {
			t.Errorf("dropped(%v) = false, want true", err)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
