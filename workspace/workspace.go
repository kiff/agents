// Package workspace is an agent's place to work during one run: a few files,
// and the commands it runs on them. A workspace holds no credentials and has
// no way out, so what happens in it changes only the agent's own state.
// Anything that reaches outside goes through KIFF's gateway, like any other
// write (#86). Each run has its own workspace, deleted once the run is done.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode"
)

// Key names one run's workspace.
type Key struct {
	Tenant string
	Run    string
}

// Check refuses a key that names no tenant or no run.
func (k Key) Check() error {
	if k.Tenant == "" || k.Run == "" {
		return errors.New("workspace: a key names a tenant and a run")
	}
	return nil
}

// Limits every Workspaces keeps.
const (
	// MaxPath bounds a file's path, in bytes.
	MaxPath = 256
	// MaxFile bounds one file, and MaxTotal all of a run's files together.
	MaxFile  = 1 << 20
	MaxTotal = 16 << 20
	// MaxFiles bounds how many files a run keeps.
	MaxFiles = 200
	// MaxOutput bounds what a command returns, stdout and stderr each. The
	// rest is cut, and Output says so.
	MaxOutput = 32 << 10
	// DefaultTimeout is how long a command runs when the caller doesn't say;
	// MaxTimeout is the most it may ask for.
	DefaultTimeout = 60 * time.Second
	MaxTimeout     = 5 * time.Minute
)

// File is one file in a workspace.
type File struct {
	Path string
	Size int64
}

// Output is what a command did. TimedOut means it was stopped at its
// timeout; Cut means stdout or stderr was longer than MaxOutput.
type Output struct {
	Stdout   string
	Stderr   string
	Exit     int
	TimedOut bool
	Cut      bool
}

var (
	// ErrNotFound means the run's workspace has no file at that path.
	ErrNotFound = errors.New("workspace: no such file")
	// ErrTooLarge means a file, or the run's files together, would pass a limit.
	ErrTooLarge = errors.New("workspace: past its size limits")
	// ErrPath means a path is not a plain relative path inside the workspace.
	ErrPath = errors.New("workspace: a path is relative, inside the workspace, and printable")
)

// Workspaces keeps each run's workspace. An implementation never lets one
// key reach another key's files, keeps the limits above, and runs commands
// with no credentials and no network.
type Workspaces interface {
	// Put writes a file, replacing one at that path.
	Put(ctx context.Context, k Key, path string, data []byte) error
	// Get reads a file; ErrNotFound when there is none.
	Get(ctx context.Context, k Key, path string) ([]byte, error)
	// List returns the run's files, by path.
	List(ctx context.Context, k Key) ([]File, error)
	// Exec runs a shell command in the run's workspace, stopping it at
	// timeout. A command that fails is an Output with its exit code, not an
	// error; an error means it couldn't be run.
	Exec(ctx context.Context, k Key, command string, timeout time.Duration) (Output, error)
	// Delete removes the run's workspace. Already gone counts as done.
	Delete(ctx context.Context, k Key) error
}

// CleanPath returns p as stored: relative, cleaned, inside the workspace.
// It refuses an absolute path, one that climbs out with "..", a backslash,
// and control characters.
func CleanPath(p string) (string, error) {
	if p == "" || len(p) > MaxPath || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) {
		return "", fmt.Errorf("%w: %q", ErrPath, p)
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: %q", ErrPath, p)
		}
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("%w: %q", ErrPath, p)
	}
	return c, nil
}

// Cut returns s at most MaxOutput bytes long, and whether it was cut. It
// never splits a UTF-8 sequence.
func Cut(s string) (string, bool) {
	if len(s) <= MaxOutput {
		return s, false
	}
	i := MaxOutput
	for i > 0 && i > MaxOutput-4 && !utf8Start(s[i]) {
		i--
	}
	return s[:i], true
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
