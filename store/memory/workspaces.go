package memory

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kiff/agents/workspace"
)

// Workspaces is a workspace.Workspaces held in memory, for tests and local
// runs. It runs no commands itself: Commands, when set, does, given a copy
// of the run's files; without it a command answers exit 127.
type Workspaces struct {
	// Commands runs a command on a copy of the run's files and returns its
	// output. The files it returns replace the run's files.
	Commands func(ctx context.Context, files map[string][]byte, command string, timeout time.Duration) (workspace.Output, map[string][]byte, error)

	mu    sync.Mutex
	files map[workspace.Key]map[string][]byte
}

// NewWorkspaces returns an empty Workspaces.
func NewWorkspaces() *Workspaces {
	return &Workspaces{files: map[workspace.Key]map[string][]byte{}}
}

func (w *Workspaces) Put(_ context.Context, k workspace.Key, p string, data []byte) error {
	if err := k.Check(); err != nil {
		return err
	}
	p, err := workspace.CleanPath(p)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	fs := w.files[k]
	if err := fits(fs, p, len(data)); err != nil {
		return err
	}
	if fs == nil {
		fs = map[string][]byte{}
		w.files[k] = fs
	}
	fs[p] = slices.Clone(data)
	return nil
}

// fits reports whether writing size bytes at p keeps fs inside the limits.
func fits(fs map[string][]byte, p string, size int) error {
	if size > workspace.MaxFile {
		return workspace.ErrTooLarge
	}
	old, exists := fs[p]
	if !exists && len(fs) >= workspace.MaxFiles {
		return workspace.ErrTooLarge
	}
	total := size - len(old)
	for _, b := range fs {
		total += len(b)
	}
	if total > workspace.MaxTotal {
		return workspace.ErrTooLarge
	}
	return nil
}

func (w *Workspaces) Get(_ context.Context, k workspace.Key, p string) ([]byte, error) {
	if err := k.Check(); err != nil {
		return nil, err
	}
	p, err := workspace.CleanPath(p)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	b, ok := w.files[k][p]
	if !ok {
		return nil, workspace.ErrNotFound
	}
	return slices.Clone(b), nil
}

func (w *Workspaces) List(_ context.Context, k workspace.Key) ([]workspace.File, error) {
	if err := k.Check(); err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	files := make([]workspace.File, 0, len(w.files[k]))
	for p, b := range w.files[k] {
		files = append(files, workspace.File{Path: p, Size: int64(len(b))})
	}
	slices.SortFunc(files, func(a, b workspace.File) int { return strings.Compare(a.Path, b.Path) })
	return files, nil
}

func (w *Workspaces) Exec(ctx context.Context, k workspace.Key, command string, timeout time.Duration) (workspace.Output, error) {
	if err := k.Check(); err != nil {
		return workspace.Output{}, err
	}
	if w.Commands == nil {
		return workspace.Output{Exit: 127, Stderr: "commands don't run in this workspace"}, nil
	}
	w.mu.Lock()
	in := map[string][]byte{}
	for p, b := range w.files[k] {
		in[p] = slices.Clone(b)
	}
	w.mu.Unlock()
	out, files, err := w.Commands(ctx, in, command, timeout)
	if err != nil {
		return workspace.Output{}, err
	}
	w.mu.Lock()
	w.files[k] = files
	w.mu.Unlock()
	out.Stdout, out.Cut = cutBoth(out.Stdout, out.Cut)
	out.Stderr, out.Cut = cutBoth(out.Stderr, out.Cut)
	return out, nil
}

func cutBoth(s string, already bool) (string, bool) {
	s, cut := workspace.Cut(s)
	return s, already || cut
}

func (w *Workspaces) Delete(_ context.Context, k workspace.Key) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.files, k)
	return nil
}
