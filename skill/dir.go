package skill

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// LoadDir reads every skill under dir for tenant: <dir>/<name>/<version>/SKILL.md,
// and the files beside it, at any depth. Each package is checked as a store
// would keep it. The eval and the end-to-end test load a checkout's skills
// this way, so the definitions they run can name them (#101).
func LoadDir(dir, tenant string) ([]Package, error) {
	versions, err := filepath.Glob(filepath.Join(dir, "*", "*", "SKILL.md"))
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("no <name>/<version>/SKILL.md under %s", dir)
	}
	var out []Package
	for _, path := range versions {
		root := filepath.Dir(path)
		md, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		name, description, body, err := Parse(string(md))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if name != filepath.Base(filepath.Dir(root)) {
			return nil, fmt.Errorf("%s names %q: its directory must be named so", path, name)
		}
		files := map[string]string{}
		err = filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || p == path {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			files[filepath.ToSlash(rel)] = string(b)
			return nil
		})
		if err != nil {
			return nil, err
		}
		p := Package{Tenant: tenant, Name: name, Version: filepath.Base(root), Description: description, Body: body}
		if len(files) > 0 {
			p.Files = files
		}
		if err := p.Check(); err != nil {
			return nil, fmt.Errorf("%s: %w", root, err)
		}
		out = append(out, p)
	}
	return out, nil
}
