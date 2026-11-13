package eval

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// The Eval runs in suites (suites.json): each a few of eval.yml's steps, by
// their names, over one area (connecting an app, setting up a taf, a taf
// within its Cards, hand-offs, each recipe). A change runs only the suites
// its paths touch; the full run, every suite, comes before an Upgrade.

//go:embed suites.json
var suitesJSON []byte

// Suites is the Eval's suites and which paths touch each.
type Suites struct {
	Suites map[string]Suite `json:"suites"`
	// Paths maps a repository's paths to the suites they touch: the first
	// prefix a path starts with decides, and none (an empty list) means the
	// path touches no suite (the apps, docs). A path no prefix covers, or a
	// repository not listed, touches every suite.
	Paths map[string][]PathSuites `json:"paths"`
}

// Suite is one suite: what it tests and eval.yml's steps that run it.
type Suite struct {
	About string   `json:"about"`
	Steps []string `json:"steps"`
}

// PathSuites is the suites a path prefix touches.
type PathSuites struct {
	Prefix string   `json:"prefix"`
	Suites []string `json:"suites"`
}

// All names every suite, as a run of them all is asked for.
const All = "all"

// LoadSuites reads suites.json and checks each path's suites are suites.
func LoadSuites() (Suites, error) {
	var s Suites
	if err := json.Unmarshal(suitesJSON, &s); err != nil {
		return Suites{}, fmt.Errorf("suites.json: %w", err)
	}
	for repo, ps := range s.Paths {
		for _, p := range ps {
			for _, name := range p.Suites {
				if _, ok := s.Suites[name]; !ok {
					return Suites{}, fmt.Errorf("suites.json: %s %q names no suite %q", repo, p.Prefix, name)
				}
			}
		}
	}
	return s, nil
}

// Names are the suites, sorted.
func (s Suites) Names() []string {
	names := make([]string, 0, len(s.Suites))
	for n := range s.Suites {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// For is the suites a change to paths in repo touches, sorted, or [All]
// when one of them touches every suite.
func (s Suites) For(repo string, paths []string) []string {
	rules, ok := s.Paths[repo]
	if !ok {
		return []string{All}
	}
	var out []string
	for _, path := range paths {
		i := slices.IndexFunc(rules, func(r PathSuites) bool { return strings.HasPrefix(path, r.Prefix) })
		if i < 0 {
			return []string{All}
		}
		for _, n := range rules[i].Suites {
			if !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	slices.Sort(out)
	return out
}

// Asked checks a run's suites, as a dispatch names them ("connect, setup",
// or "all" or nothing for every one), and returns them as eval.yml's steps
// test them: ",connect,setup," or ",all,".
func (s Suites) Asked(asked string) (string, error) {
	var names []string
	for _, n := range strings.FieldsFunc(asked, func(r rune) bool { return r == ',' || r == ' ' }) {
		if n == All {
			return "," + All + ",", nil
		}
		if _, ok := s.Suites[n]; !ok {
			return "", fmt.Errorf("no suite %q: the suites are %s, or all", n, strings.Join(s.Names(), ", "))
		}
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return "," + All + ",", nil
	}
	slices.Sort(names)
	return "," + strings.Join(names, ",") + ",", nil
}
