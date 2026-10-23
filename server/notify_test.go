package server_test

import (
	"testing"

	"github.com/kiff/agents/run"
)

// A run started with notify is announced at a URL the server allows, with
// the caller's ref; any other URL, or a short ref, is refused before the run
// starts (kiff/tafs#206).
func TestARunIsAnnouncedOnlyWhereTheServerAllows(t *testing.T) {
	f := newRunFixture(t)
	f.server.NotifyURLs = []string{"https://tafs.test/v1/runs/ended"}
	f.restart()
	f.putAgent("t1", "refunds")
	start := func(url, ref string) map[string]any {
		return map[string]any{"input": "x", "background": true, "notify": map[string]any{"url": url, "ref": ref}}
	}

	code, body := f.do("t1", "POST", "/agents/refunds/runs", start("https://elsewhere.test/v1/runs/ended", "ref-0123456789abcdef"))
	f.want(400, code, body, "a URL the server doesn't allow")
	code, body = f.do("t1", "POST", "/agents/refunds/runs", start("https://tafs.test/v1/runs/ended", "short"))
	f.want(400, code, body, "a short ref")
	if n := len(f.runner.starts); n != 0 {
		t.Fatalf("a refused run started: %d starts", n)
	}

	code, body = f.do("t1", "POST", "/agents/refunds/runs", start("https://tafs.test/v1/runs/ended", "ref-0123456789abcdef"))
	f.want(202, code, body, "an allowed URL")
	want := run.Notify{URL: "https://tafs.test/v1/runs/ended", Ref: "ref-0123456789abcdef"}
	if n := len(f.runner.starts); n != 1 || f.runner.starts[0].Notify != want {
		t.Fatalf("starts %+v: want one, with its notify", f.runner.starts)
	}
}
