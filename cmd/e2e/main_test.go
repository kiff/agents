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
