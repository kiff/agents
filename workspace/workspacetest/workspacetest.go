// Package workspacetest checks that a workspace.Workspaces keeps the promises
// the workspace package makes. Every backend runs the same checks:
//
//	func TestWorkspaces(t *testing.T) {
//		workspacetest.Run(t, func(t *testing.T) workspace.Workspaces { return newBackend(t) })
//	}
//
// The checks cover files, limits and isolation. Commands are each adapter's
// own to test: how a command is kept from the network is where adapters
// differ.
package workspacetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/kiff/agents/workspace"
)

var ctx = context.Background()

// Run runs every check, each against a fresh backend from newBackend.
func Run(t *testing.T, newBackend func(t *testing.T) workspace.Workspaces) {
	checks := map[string]func(*testing.T, workspace.Workspaces){
		"a file reads back as written":           readsBack,
		"a write replaces the file":              replaces,
		"a missing file is ErrNotFound":          notFound,
		"files are listed by path":               listsByPath,
		"one key never reaches another's":        isolated,
		"paths are cleaned and checked":          paths,
		"a file past MaxFile is refused":         fileLimit,
		"files past MaxTotal are refused":        totalLimit,
		"files past MaxFiles are refused":        countLimit,
		"delete removes only that workspace":     deletes,
		"a key with no tenant or run is refused": badKey,
	}
	for name, check := range checks {
		t.Run(name, func(t *testing.T) { check(t, newBackend(t)) })
	}
}

var k1 = workspace.Key{Tenant: "t1", Run: "r1"}

func put(t *testing.T, w workspace.Workspaces, k workspace.Key, p, data string) {
	t.Helper()
	if err := w.Put(ctx, k, p, []byte(data)); err != nil {
		t.Fatalf("Put %s: %v", p, err)
	}
}

func readsBack(t *testing.T, w workspace.Workspaces) {
	put(t, w, k1, "relay/config.yaml", "region: eu-west-1\n")
	got, err := w.Get(ctx, k1, "relay/config.yaml")
	if err != nil || string(got) != "region: eu-west-1\n" {
		t.Fatalf("Get = %q, %v", got, err)
	}
}

func replaces(t *testing.T, w workspace.Workspaces) {
	put(t, w, k1, "a.txt", "one")
	put(t, w, k1, "a.txt", "two")
	if got, _ := w.Get(ctx, k1, "a.txt"); string(got) != "two" {
		t.Fatalf("Get = %q, want the second write", got)
	}
	if files, _ := w.List(ctx, k1); len(files) != 1 {
		t.Fatalf("List = %v, want one file", files)
	}
}

func notFound(t *testing.T, w workspace.Workspaces) {
	if _, err := w.Get(ctx, k1, "nope.txt"); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("Get of a missing file: %v, want ErrNotFound", err)
	}
}

func listsByPath(t *testing.T, w workspace.Workspaces) {
	put(t, w, k1, "b.txt", "bb")
	put(t, w, k1, "a/c.txt", "c")
	put(t, w, k1, "a.txt", "aaa")
	files, err := w.List(ctx, k1)
	if err != nil {
		t.Fatal(err)
	}
	want := []workspace.File{{Path: "a.txt", Size: 3}, {Path: "a/c.txt", Size: 1}, {Path: "b.txt", Size: 2}}
	if fmt.Sprint(files) != fmt.Sprint(want) {
		t.Fatalf("List = %v, want %v", files, want)
	}
}

func isolated(t *testing.T, w workspace.Workspaces) {
	put(t, w, k1, "secret.txt", "only r1")
	for _, other := range []workspace.Key{{Tenant: "t2", Run: "r1"}, {Tenant: "t1", Run: "r2"}} {
		if _, err := w.Get(ctx, other, "secret.txt"); !errors.Is(err, workspace.ErrNotFound) {
			t.Errorf("%+v read r1's file: %v", other, err)
		}
		if files, err := w.List(ctx, other); err != nil || len(files) != 0 {
			t.Errorf("%+v listed %v, %v", other, files, err)
		}
	}
}

func paths(t *testing.T, w workspace.Workspaces) {
	put(t, w, k1, "./dir//x.txt", "x")
	if got, err := w.Get(ctx, k1, "dir/x.txt"); err != nil || string(got) != "x" {
		t.Fatalf("the cleaned path reads %q, %v", got, err)
	}
	for _, p := range []string{"../escape.txt", "/abs.txt", "a/../../b"} {
		if err := w.Put(ctx, k1, p, []byte("x")); !errors.Is(err, workspace.ErrPath) {
			t.Errorf("Put %q: %v, want ErrPath", p, err)
		}
		if _, err := w.Get(ctx, k1, p); !errors.Is(err, workspace.ErrPath) {
			t.Errorf("Get %q: %v, want ErrPath", p, err)
		}
	}
}

func fileLimit(t *testing.T, w workspace.Workspaces) {
	if err := w.Put(ctx, k1, "ok.bin", make([]byte, workspace.MaxFile)); err != nil {
		t.Fatalf("a file of exactly MaxFile: %v", err)
	}
	if err := w.Put(ctx, k1, "big.bin", make([]byte, workspace.MaxFile+1)); !errors.Is(err, workspace.ErrTooLarge) {
		t.Fatalf("a file past MaxFile: %v, want ErrTooLarge", err)
	}
}

func totalLimit(t *testing.T, w workspace.Workspaces) {
	n := workspace.MaxTotal / workspace.MaxFile
	for i := range n {
		if err := w.Put(ctx, k1, fmt.Sprintf("f%d.bin", i), make([]byte, workspace.MaxFile)); err != nil {
			t.Fatalf("file %d of %d: %v", i, n, err)
		}
	}
	if err := w.Put(ctx, k1, "one-more.bin", []byte("x")); !errors.Is(err, workspace.ErrTooLarge) {
		t.Fatalf("a byte past MaxTotal: %v, want ErrTooLarge", err)
	}
	// Replacing a file counts only the difference.
	if err := w.Put(ctx, k1, "f0.bin", []byte("small")); err != nil {
		t.Fatalf("shrinking a file at the limit: %v", err)
	}
	// Another run is not counted against this one.
	if err := w.Put(ctx, workspace.Key{Tenant: "t1", Run: "r2"}, "a.txt", []byte("x")); err != nil {
		t.Fatalf("another run's first file: %v", err)
	}
}

func countLimit(t *testing.T, w workspace.Workspaces) {
	for i := range workspace.MaxFiles {
		put(t, w, k1, fmt.Sprintf("n%d.txt", i), "x")
	}
	if err := w.Put(ctx, k1, "extra.txt", []byte("x")); !errors.Is(err, workspace.ErrTooLarge) {
		t.Fatalf("a file past MaxFiles: %v, want ErrTooLarge", err)
	}
	put(t, w, k1, "n0.txt", "replacing is fine")
}

func deletes(t *testing.T, w workspace.Workspaces) {
	k2 := workspace.Key{Tenant: "t1", Run: "r2"}
	put(t, w, k1, "a.txt", "1")
	put(t, w, k2, "a.txt", "2")
	if err := w.Delete(ctx, k1); err != nil {
		t.Fatal(err)
	}
	if err := w.Delete(ctx, k1); err != nil {
		t.Fatalf("deleting twice: %v", err)
	}
	if files, _ := w.List(ctx, k1); len(files) != 0 {
		t.Fatalf("after Delete, List = %v", files)
	}
	if got, _ := w.Get(ctx, k2, "a.txt"); !bytes.Equal(got, []byte("2")) {
		t.Fatalf("Delete of r1 touched r2: %q", got)
	}
}

func badKey(t *testing.T, w workspace.Workspaces) {
	for _, k := range []workspace.Key{{}, {Tenant: "t1"}, {Run: "r1"}} {
		if err := w.Put(ctx, k, "a.txt", []byte("x")); err == nil {
			t.Errorf("Put with %+v succeeded", k)
		}
		if _, err := w.List(ctx, k); err == nil {
			t.Errorf("List with %+v succeeded", k)
		}
	}
}
