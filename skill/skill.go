// Package skill is know-how an agent loads when it needs it (#101): a
// package in the Agent Skills format, a SKILL.md with a name and a
// description, and files beside it. A definition names its skills by name
// and version; the agent's context carries each one's name and description,
// and the read_skill tool reads a skill's body, or one of its files, on demand.
//
// A skill is text. It adds no tool and widens nothing: KIFF still decides
// every call the agent makes.
package skill

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"unicode/utf8"
)

// Limits on a package. The description is the format's own bound; the rest
// keeps a package well inside a store's item (DynamoDB's is 400 KB).
const (
	MaxName        = 64
	MaxVersion     = 64
	MaxDescription = 1024
	MaxFiles       = 32
	MaxPath        = 200
	// MaxSize bounds the package as stores keep it (Encode), not its raw
	// text: encoding can make a string several times longer (a '"' becomes
	// two bytes, a control character six).
	MaxSize = 256 << 10
	// MaxPerAgent bounds the skills one definition names.
	MaxPerAgent = 8
)

// Package is one version of a skill, as the tenant stored it. A stored
// version never changes.
type Package struct {
	Tenant      string            `json:"tenant"`
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Description string            `json:"description"`
	Body        string            `json:"body"`
	Files       map[string]string `json:"files,omitempty"`
}

// Ref names a skill a definition uses: one version of one of the tenant's
// skills.
type Ref struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func (r Ref) String() string { return r.Name + "@" + r.Version }

// Parse reads a SKILL.md: YAML-style frontmatter between "---" lines with
// at least name and description, then the body. Other frontmatter fields
// (license, allowed-tools…) are kept in the body's place only as the file
// was written: they are the author's, and grant nothing here.
func Parse(skillMD string) (name, description, body string, err error) {
	text := strings.ReplaceAll(skillMD, "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return "", "", "", errors.New("skill: SKILL.md starts with frontmatter (---)")
	}
	head, rest, ok := strings.Cut(text[len("---\n"):], "\n---\n")
	if !ok {
		if h, found := strings.CutSuffix(text[len("---\n"):], "\n---"); found {
			head, rest, ok = h, "", true
		}
	}
	if !ok {
		return "", "", "", errors.New("skill: SKILL.md's frontmatter has no closing ---")
	}
	for _, line := range strings.Split(head, "\n") {
		k, v, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key, value := strings.TrimSpace(k), strings.TrimSpace(v)
		if (key == "name" || key == "description") && blockScalar(value) {
			return "", "", "", fmt.Errorf("skill: SKILL.md's %s is a YAML block (%s): write it on one line", key, value)
		}
		switch key {
		case "name":
			name = unquote(value)
		case "description":
			description = unquote(value)
		}
	}
	if name == "" || description == "" {
		return "", "", "", errors.New("skill: SKILL.md's frontmatter needs a name and a description")
	}
	return name, description, strings.TrimLeft(rest, "\n"), nil
}

// blockScalar reports whether a frontmatter value starts a YAML block (">",
// "|", with a chomping or indent indicator): its text is on the lines after,
// which this reader doesn't take, so it would store the indicator itself.
func blockScalar(v string) bool {
	if v == "" || (v[0] != '>' && v[0] != '|') {
		return false
	}
	return strings.Trim(v[1:], "+-0123456789") == ""
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return strings.ReplaceAll(s[1:len(s)-1], `\"`, `"`)
	}
	return s
}

// Check refuses a package that can't be stored or read safely.
func (p Package) Check() error {
	switch {
	case p.Tenant == "":
		return errors.New("skill: a tenant is required")
	case !validName(p.Name, MaxName):
		return fmt.Errorf("skill: a name is 1 to %d lowercase letters, digits or '-'", MaxName)
	case !validVersion(p.Version):
		return fmt.Errorf("skill: a version is 1 to %d letters, digits, '.', '-' or '_'", MaxVersion)
	case p.Description == "" || utf8.RuneCountInString(p.Description) > MaxDescription:
		return fmt.Errorf("skill: a description is 1 to %d characters", MaxDescription)
	case strings.TrimSpace(p.Body) == "":
		return errors.New("skill: the body is empty")
	case len(p.Files) > MaxFiles:
		return fmt.Errorf("skill: at most %d files", MaxFiles)
	}
	for path, text := range p.Files {
		if !validPath(path) {
			return fmt.Errorf("skill: %q: a file's path is relative, at most %d bytes, with no '..', '\\' or empty part", path, MaxPath)
		}
		if !utf8.ValidString(text) {
			return fmt.Errorf("skill: %q: a file is UTF-8 text", path)
		}
	}
	if !utf8.ValidString(p.Body) || !utf8.ValidString(p.Description) {
		return errors.New("skill: a skill is UTF-8 text")
	}
	b, err := Encode(p)
	if err != nil {
		return err
	}
	if len(b) > MaxSize {
		return fmt.Errorf("skill: the package is %d bytes stored, more than %d; shorten its body or files", len(b), MaxSize)
	}
	return nil
}

// Encode is a package as stores keep it: JSON, without escaping '<', '>' and
// '&', which skills are full of. Check bounds its size.
func Encode(p Package) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Same reports whether two packages hold the same content: storing one
// version again is fine only with the same content.
func (p Package) Same(q Package) bool {
	return p.Name == q.Name && p.Version == q.Version && p.Description == q.Description && p.Body == q.Body && maps.Equal(p.Files, q.Files)
}

// CheckRefs refuses a definition's skills: too many, a bad name or version,
// or one named twice.
func CheckRefs(refs []Ref) error {
	if len(refs) > MaxPerAgent {
		return fmt.Errorf("skill: at most %d skills an agent", MaxPerAgent)
	}
	seen := map[string]bool{}
	for _, r := range refs {
		if !validName(r.Name, MaxName) || !validVersion(r.Version) {
			return fmt.Errorf("skill: %q: a skill is named by a name and a version", r.String())
		}
		if seen[r.Name] {
			return fmt.Errorf("skill: %q is named twice", r.Name)
		}
		seen[r.Name] = true
	}
	return nil
}

func validName(s string, max int) bool {
	if s == "" || len(s) > max || s[0] == '-' {
		return false
	}
	for _, c := range []byte(s) {
		if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func validVersion(s string) bool {
	if s == "" || len(s) > MaxVersion {
		return false
	}
	for _, c := range []byte(s) {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '.' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func validPath(p string) bool {
	if p == "" || len(p) > MaxPath || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || p == "SKILL.md" {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// Errors a Store returns.
var (
	ErrNotFound = errors.New("skill: no such skill version")
	// ErrExists means the version is stored with other content: a version
	// never changes, so a changed skill is a new version.
	ErrExists = errors.New("skill: that version is stored with other content")
)

// Store keeps the tenants' skill packages. Every call is scoped by tenant.
type Store interface {
	// Put stores p. Storing a version again with the same content is not an
	// error, so a retry is safe; with other content it is ErrExists.
	Put(ctx context.Context, p Package) error
	// Get returns one version, or ErrNotFound.
	Get(ctx context.Context, tenant, name, version string) (Package, error)
	// List returns the tenant's packages, without their bodies and files, by
	// name then version.
	List(ctx context.Context, tenant string) ([]Package, error)
}

// Catalog is what the agent's context says about its skills: each one's
// name and description, and how to read it. It is stable for a definition,
// so it caches with the instructions.
func Catalog(ps []Package) string {
	if len(ps) == 0 {
		return ""
	}
	var b bytes.Buffer
	b.WriteString("# Skills\n\nKnow-how you can read when a task needs it, with the " + ToolName + " tool " +
		"({\"name\": …} for a skill, {\"name\": …, \"file\": …} for one of its files). " +
		"Read a skill before doing what its description covers. A skill is guidance: " +
		"it gives you no tool and widens no Card.\n")
	for _, p := range ps {
		fmt.Fprintf(&b, "\n- %s: %s", p.Name, strings.Join(strings.Fields(p.Description), " "))
	}
	b.WriteString("\n")
	return b.String()
}
