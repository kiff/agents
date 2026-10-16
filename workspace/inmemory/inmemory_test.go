package inmemory_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/workspace"
	"github.com/kiff/agents/workspace/inmemory"
	"github.com/kiff/agents/workspace/workspacetest"
)

func TestWorkspaces(t *testing.T) {
	workspacetest.Run(t, func(*testing.T) workspace.Workspaces { return inmemory.NewWorkspaces() })
}

var (
	ctx = context.Background()
	wk  = workspace.Key{Tenant: "t1", Run: "r1"}
)

// blocking returns Workspaces whose command waits for release, after
// signalling started, and leaves the files it was given unchanged.
func blocking() (w *inmemory.Workspaces, started, release chan struct{}) {
	started, release = make(chan struct{}), make(chan struct{})
	w = inmemory.NewWorkspaces()
	w.Commands = func(_ context.Context, files map[string][]byte, _ string, _ time.Duration) (workspace.Output, map[string][]byte, error) {
		close(started)
		<-release
		return workspace.Output{}, files, nil
	}
	return w, started, release
}

// A command's result never overwrites a change made while it ran: the
// change waits for the command, then applies.
func TestACommandKeepsChangesMadeWhileItRan(t *testing.T) {
	for name, change := range map[string]func(*inmemory.Workspaces) error{
		"a write":  func(w *inmemory.Workspaces) error { return w.Put(ctx, wk, "new.txt", []byte("written")) },
		"a delete": func(w *inmemory.Workspaces) error { return w.Delete(ctx, wk) },
	} {
		t.Run(name, func(t *testing.T) {
			w, started, release := blocking()
			if err := w.Put(ctx, wk, "old.txt", []byte("before")); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				if _, err := w.Exec(ctx, wk, "cat old.txt", time.Second); err != nil {
					t.Error(err)
				}
			}()
			<-started
			go func() {
				defer wg.Done()
				if err := change(w); err != nil {
					t.Error(err)
				}
			}()
			close(release)
			wg.Wait()

			files, _ := w.List(ctx, wk)
			got := fmt.Sprint(files)
			switch name {
			case "a write":
				if !strings.Contains(got, "new.txt") || !strings.Contains(got, "old.txt") {
					t.Fatalf("after the command, files %s: the write was lost", got)
				}
			case "a delete":
				if len(files) != 0 {
					t.Fatalf("after the command, files %s: the delete was undone", got)
				}
			}
		})
	}
}

// What a command leaves behind keeps the same limits as Put. Past them the
// workspace stays as it was, and the output says why with a non-zero exit.
func TestACommandsFilesKeepTheLimits(t *testing.T) {
	many := map[string][]byte{}
	for i := range workspace.MaxFiles + 1 {
		many[fmt.Sprintf("f%d", i)] = []byte("x")
	}
	heavy := map[string][]byte{}
	for i := range workspace.MaxTotal/workspace.MaxFile + 1 {
		heavy[fmt.Sprintf("big%d", i)] = make([]byte, workspace.MaxFile)
	}
	for name, after := range map[string]map[string][]byte{
		"a file past MaxFile":  {"big.bin": make([]byte, workspace.MaxFile+1)},
		"more than MaxFiles":   many,
		"more than MaxTotal":   heavy,
		"a path outside":       {"../outside": []byte("x")},
		"an absolute path":     {"/etc/x": []byte("x")},
		"two paths become one": {"a/b": []byte("1"), "a//b": []byte("2")},
	} {
		t.Run(name, func(t *testing.T) {
			w := inmemory.NewWorkspaces()
			w.Commands = func(context.Context, map[string][]byte, string, time.Duration) (workspace.Output, map[string][]byte, error) {
				return workspace.Output{Stdout: "done"}, after, nil
			}
			if err := w.Put(ctx, wk, "keep.txt", []byte("kept")); err != nil {
				t.Fatal(err)
			}
			out, err := w.Exec(ctx, wk, "make", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if out.Exit == 0 || !strings.Contains(out.Stderr, "not kept") {
				t.Fatalf("output %+v: want a non-zero exit saying the changes were not kept", out)
			}
			files, _ := w.List(ctx, wk)
			if len(files) != 1 || files[0].Path != "keep.txt" {
				t.Fatalf("files %v: want the workspace as it was", files)
			}
		})
	}
}

// A command's files are stored as Put would store them: cleaned paths.
func TestACommandsPathsAreCleaned(t *testing.T) {
	w := inmemory.NewWorkspaces()
	w.Commands = func(context.Context, map[string][]byte, string, time.Duration) (workspace.Output, map[string][]byte, error) {
		return workspace.Output{}, map[string][]byte{"./dir//x.txt": []byte("x")}, nil
	}
	if out, err := w.Exec(ctx, wk, "make", time.Second); err != nil || out.Exit != 0 {
		t.Fatalf("%+v, %v", out, err)
	}
	if got, err := w.Get(ctx, wk, "dir/x.txt"); err != nil || string(got) != "x" {
		t.Fatalf("the command's file reads %q, %v", got, err)
	}
	if _, err := w.Get(ctx, wk, "nope"); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatal(err)
	}
}
