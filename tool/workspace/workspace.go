// Package workspace is the tool through which an agent works in its run's
// workspace: it writes and reads files, and runs commands on them. The
// workspace has no credentials and no way out, so the tool changes only the
// agent's own state and doesn't go through KIFF's gateway. Anything that
// reaches outside is another tool, through the gateway (#86).
package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/workspace"
)

// Name is the tool's name, as the model sees it.
const Name = "workspace"

// Tool is one run's workspace.
type Tool struct {
	ws  workspace.Workspaces
	key workspace.Key
}

// New returns the workspace tool for one run.
func New(ws workspace.Workspaces, k workspace.Key) *Tool { return &Tool{ws: ws, key: k} }

func (t *Tool) Access() tool.Access { return tool.Own }
func (t *Tool) Gateway() bool       { return false }

func (t *Tool) Spec() model.ToolSpec {
	return model.ToolSpec{
		Name: Name,
		Description: "Your workspace for this run: files, and commands on them. " +
			"It has no network and no credentials; to reach anything outside, use your other tools. " +
			"It is deleted when the run ends. " +
			"write: save a file (replaces one at that path); read: a file's text; list: your files; " +
			fmt.Sprintf("run: a shell command in the workspace, stopped after timeout_seconds (default %d, at most %d).",
				int(workspace.DefaultTimeout.Seconds()), int(workspace.MaxTimeout.Seconds())),
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"action":{"type":"string","enum":["write","read","list","run"]},` +
			`"path":{"type":"string","description":"a relative path in the workspace (write, read)"},` +
			`"text":{"type":"string","description":"the file's contents (write)"},` +
			`"command":{"type":"string","description":"a shell command (run)"},` +
			`"timeout_seconds":{"type":"integer","description":"how long run may take"}},` +
			`"required":["action"]}`),
	}
}

// Call writes, reads, lists or runs. A bad request, a missing file or a
// limit is a Result with IsError, so the model can correct it.
func (t *Tool) Call(ctx context.Context, input json.RawMessage) (tool.Result, error) {
	var in struct {
		Action  string `json:"action"`
		Path    string `json:"path"`
		Text    string `json:"text"`
		Command string `json:"command"`
		Timeout int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return refuse("the input must be {action, path, text, command, timeout_seconds}"), nil
	}
	switch in.Action {
	case "write":
		if err := t.ws.Put(ctx, t.key, in.Path, []byte(in.Text)); err != nil {
			return mistake(err)
		}
		return tool.Result{Text: fmt.Sprintf("Wrote %s (%d bytes).", in.Path, len(in.Text))}, nil
	case "read":
		b, err := t.ws.Get(ctx, t.key, in.Path)
		if err != nil {
			return mistake(err)
		}
		if !utf8.Valid(b) {
			return tool.Result{Text: fmt.Sprintf("%s is %d bytes and not text.", in.Path, len(b))}, nil
		}
		s, cut := workspace.Cut(string(b))
		if cut {
			s += fmt.Sprintf("\n[cut: the file is %d bytes; this shows the first %d]", len(b), len(s))
		}
		return tool.Result{Text: s}, nil
	case "list":
		files, err := t.ws.List(ctx, t.key)
		if err != nil {
			return tool.Result{}, err
		}
		if len(files) == 0 {
			return tool.Result{Text: "The workspace is empty."}, nil
		}
		var b strings.Builder
		for _, f := range files {
			fmt.Fprintf(&b, "%s\t%d bytes\n", f.Path, f.Size)
		}
		return tool.Result{Text: strings.TrimSuffix(b.String(), "\n")}, nil
	case "run":
		if strings.TrimSpace(in.Command) == "" {
			return refuse("run needs a command"), nil
		}
		timeout := workspace.DefaultTimeout
		if in.Timeout != 0 {
			timeout = time.Duration(in.Timeout) * time.Second
		}
		if timeout <= 0 || timeout > workspace.MaxTimeout {
			return refuse(fmt.Sprintf("timeout_seconds is 1 to %d", int(workspace.MaxTimeout.Seconds()))), nil
		}
		out, err := t.ws.Exec(ctx, t.key, in.Command, timeout)
		if err != nil {
			return tool.Result{}, err
		}
		return tool.Result{Text: report(out, timeout), IsError: out.Exit != 0 || out.TimedOut}, nil
	default:
		return refuse("action is write, read, list or run"), nil
	}
}

// report is a command's output as the model reads it.
func report(out workspace.Output, timeout time.Duration) string {
	var b strings.Builder
	switch {
	case out.TimedOut:
		fmt.Fprintf(&b, "stopped after %s (its timeout)\n", timeout)
	default:
		fmt.Fprintf(&b, "exit %d\n", out.Exit)
	}
	if out.Stdout != "" {
		b.WriteString("--- stdout\n" + out.Stdout + "\n")
	}
	if out.Stderr != "" {
		b.WriteString("--- stderr\n" + out.Stderr + "\n")
	}
	if out.Cut {
		fmt.Fprintf(&b, "[cut: output past %d bytes was dropped]\n", workspace.MaxOutput)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func refuse(why string) tool.Result { return tool.Result{Text: why, IsError: true} }

// mistake turns the model's own mistakes into a Result it can act on; any
// other error is the workspace failing.
func mistake(err error) (tool.Result, error) {
	switch {
	case errors.Is(err, workspace.ErrNotFound):
		return refuse("no such file; list shows what's there"), nil
	case errors.Is(err, workspace.ErrPath):
		return refuse("a path is relative, inside the workspace, and at most " + fmt.Sprint(workspace.MaxPath) + " bytes"), nil
	case errors.Is(err, workspace.ErrTooLarge):
		return refuse(fmt.Sprintf("past the workspace's limits: %d bytes a file, %d bytes and %d files in all",
			workspace.MaxFile, workspace.MaxTotal, workspace.MaxFiles)), nil
	}
	return tool.Result{}, err
}
