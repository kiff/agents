package main

import (
	"bytes"
	"fmt"
	"testing"
)

// The nightly log is public: neither the account nor the vendor's address
// may reach it, whole or as a bare host.
func TestTheLogHidesTheAccountAndTheVendor(t *testing.T) {
	var b bytes.Buffer
	out := &redacting{w: &b}
	out.hide("https://abc123.lambda-url.example/mcp", "<the vendor>/mcp")
	out.hide("abc123.lambda-url.example", "<the vendor>")
	out.hide("t_0123456789", "<the account>")
	fmt.Fprintln(out, "server https://abc123.lambda-url.example/mcp, sign in at abc123.lambda-url.example, account t_0123456789")
	if got, want := b.String(), "server <the vendor>/mcp, sign in at <the vendor>, account <the account>\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// In CI the runner masks each value too, and only the steps are written.
func TestInCIOnlyTheStepsAreWrittenAndEveryIdIsMasked(t *testing.T) {
	var b bytes.Buffer
	out := &redacting{w: &b, ci: true}
	out.hide("grt_abc", "<a grant>")
	tt := &test{out: out, cfg: config{log: out}}
	tt.logf("grant %s approved", "grt_abc")
	fmt.Fprintln(out, "FAIL: the grant grt_abc: 409")
	if got, want := b.String(), "::add-mask::grt_abc\nFAIL: the grant <a grant>: 409\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
