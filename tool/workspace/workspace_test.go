package workspace_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
	toolws "github.com/kiff/agents/tool/workspace"
	"github.com/kiff/agents/workspace"
)

var ctx = context.Background()

func call(t *testing.T, tl *toolws.Tool, input string) tool.Result {
	t.Helper()
	res, err := tl.Call(ctx, json.RawMessage(input))
	if err != nil {
		t.Fatalf("Call(%s): %v", input, err)
	}
	return res
}

func TestItIsTheAgentsOwnAndPassesCheck(t *testing.T) {
	tl := toolws.New(memory.NewWorkspaces(), workspace.Key{Tenant: "t1", Run: "r1"})
	if tl.Access() != tool.Own || tl.Gateway() {
		t.Fatalf("access %v, gateway %v: want Own, not through the gateway", tl.Access(), tl.Gateway())
	}
	if err := tool.Check([]tool.Tool{tl}); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(tl.Spec().InputSchema) || tl.Spec().Name != "workspace" {
		t.Fatalf("spec %+v", tl.Spec())
	}
}

func TestWriteReadList(t *testing.T) {
	tl := toolws.New(memory.NewWorkspaces(), workspace.Key{Tenant: "t1", Run: "r1"})
	if res := call(t, tl, `{"action":"list"}`); res.Text != "The workspace is empty." {
		t.Fatalf("empty list: %q", res.Text)
	}
	if res := call(t, tl, `{"action":"write","path":"relay/config.yaml","text":"region: eu\n"}`); res.IsError {
		t.Fatalf("write: %q", res.Text)
	}
	if res := call(t, tl, `{"action":"read","path":"relay/config.yaml"}`); res.IsError || res.Text != "region: eu\n" {
		t.Fatalf("read: %+v", res)
	}
	if res := call(t, tl, `{"action":"list"}`); res.Text != "relay/config.yaml\t11 bytes" {
		t.Fatalf("list: %q", res.Text)
	}
}

func TestTheModelsMistakesAreResults(t *testing.T) {
	tl := toolws.New(memory.NewWorkspaces(), workspace.Key{Tenant: "t1", Run: "r1"})
	for input, want := range map[string]string{
		`not json`:                                              "the input must be",
		`{"action":"delete"}`:                                   "action is write, read, list or run",
		`{"action":"read","path":"missing.txt"}`:                "no such file",
		`{"action":"write","path":"../x","text":"x"}`:           "a path is relative",
		`{"action":"run","command":"  "}`:                       "run needs a command",
		`{"action":"run","command":"ls","timeout_seconds":301}`: "timeout_seconds is 1 to 300",
		`{"action":"run","command":"ls","timeout_seconds":-1}`:  "timeout_seconds is 1 to 300",
	} {
		res := call(t, tl, input)
		if !res.IsError || !strings.Contains(res.Text, want) {
			t.Errorf("%s: %+v, want an error result containing %q", input, res, want)
		}
	}
	big := strings.Repeat("x", workspace.MaxFile+1)
	if res := call(t, tl, `{"action":"write","path":"big.txt","text":"`+big+`"}`); !res.IsError || !strings.Contains(res.Text, "limits") {
		t.Errorf("a file past MaxFile: %+v", res.Text[:min(len(res.Text), 80)])
	}
}

func TestRunReportsTheCommand(t *testing.T) {
	ws := memory.NewWorkspaces()
	var gotTimeout time.Duration
	ws.Commands = func(_ context.Context, files map[string][]byte, command string, timeout time.Duration) (workspace.Output, map[string][]byte, error) {
		gotTimeout = timeout
		switch command {
		case "make":
			files["out.txt"] = []byte("built")
			return workspace.Output{Stdout: "ok"}, files, nil
		case "fail":
			return workspace.Output{Exit: 2, Stderr: "boom"}, files, nil
		case "slow":
			return workspace.Output{TimedOut: true, Exit: -1}, files, nil
		default:
			return workspace.Output{Stdout: strings.Repeat("y", workspace.MaxOutput+10)}, files, nil
		}
	}
	tl := toolws.New(ws, workspace.Key{Tenant: "t1", Run: "r1"})

	res := call(t, tl, `{"action":"run","command":"make"}`)
	if res.IsError || res.Text != "exit 0\n--- stdout\nok" || gotTimeout != workspace.DefaultTimeout {
		t.Fatalf("make: %+v (timeout %s)", res, gotTimeout)
	}
	if res := call(t, tl, `{"action":"read","path":"out.txt"}`); res.Text != "built" {
		t.Fatalf("the command's file: %q", res.Text)
	}
	if res := call(t, tl, `{"action":"run","command":"fail","timeout_seconds":5}`); !res.IsError || res.Text != "exit 2\n--- stderr\nboom" || gotTimeout != 5*time.Second {
		t.Fatalf("fail: %+v (timeout %s)", res, gotTimeout)
	}
	if res := call(t, tl, `{"action":"run","command":"slow"}`); !res.IsError || !strings.HasPrefix(res.Text, "stopped after 1m0s") {
		t.Fatalf("slow: %+v", res)
	}
	if res := call(t, tl, `{"action":"run","command":"loud"}`); !strings.HasSuffix(res.Text, "dropped]") || len(res.Text) > workspace.MaxOutput+200 {
		t.Fatalf("loud: %d bytes, ends %q", len(res.Text), res.Text[len(res.Text)-40:])
	}
}

func TestNoCommandRunnerSaysSo(t *testing.T) {
	tl := toolws.New(memory.NewWorkspaces(), workspace.Key{Tenant: "t1", Run: "r1"})
	if res := call(t, tl, `{"action":"run","command":"ls"}`); !res.IsError || !strings.Contains(res.Text, "exit 127") {
		t.Fatalf("%+v", res)
	}
}
