package server_test

import (
	"testing"

	"github.com/kiff/agents/server"
)

// Where new runs' turns go is read from the one place that decides it, for
// the AI consent to name the company. Unset, the read is unavailable, so the
// apps show nothing rather than a guess.
func TestWhereTurnsRunIsRead(t *testing.T) {
	f := newRunFixture(t)
	code, body := f.do("t1", "GET", "/ai/source", nil)
	f.want(503, code, body, "unset")
	f.server.AISource = server.AISource{Source: "bedrock", Geo: "eu"}
	f.restart()
	code, body = f.do("t1", "GET", "/ai/source", nil)
	f.want(200, code, body, "set")
	if len(body) != 2 || body["source"] != "bedrock" || body["geo"] != "eu" {
		t.Fatalf("read %v, want the source and its geo, and no fallback", body)
	}
}
