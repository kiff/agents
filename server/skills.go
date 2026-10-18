package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/kiff/agents/skill"
)

// skillBody is what PUT /skills/{name}/{version} takes: the SKILL.md, whose
// frontmatter names the skill and describes it, and the files beside it.
type skillBody struct {
	SkillMD string            `json:"skill_md"`
	Files   map[string]string `json:"files,omitempty"`
}

// putSkill stores one version of a skill for the caller's tenant (#101). The
// SKILL.md's name must be the path's. A version never changes: storing it
// again with the same content is 200, with other content 409.
func (s *Server) putSkill(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	if s.Skills == nil {
		return 0, nil, skill.ErrNotFound
	}
	var body skillBody
	if err := decode(r, &body); err != nil {
		return 0, nil, err
	}
	name, description, text, err := skill.Parse(body.SkillMD)
	if err != nil {
		return 0, nil, badRequest{err}
	}
	if name != r.PathValue("name") {
		return 0, nil, bad("the SKILL.md names %q, not the path's %q", name, r.PathValue("name"))
	}
	p := skill.Package{Tenant: c.Tenant, Name: name, Version: r.PathValue("version"), Description: description, Body: text, Files: body.Files}
	if err := p.Check(); err != nil {
		return 0, nil, badRequest{err}
	}
	if err := s.Skills.Put(ctx, p); err != nil {
		if errors.Is(err, skill.ErrExists) {
			return 0, nil, conflict(fmt.Sprintf("%s@%s is stored with other content: a version never changes, so store a new one", p.Name, p.Version))
		}
		return 0, nil, err
	}
	return http.StatusOK, skillView(p), nil
}

// getSkill returns one version, whole.
func (s *Server) getSkill(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	if s.Skills == nil {
		return 0, nil, skill.ErrNotFound
	}
	p, err := s.Skills.Get(ctx, c.Tenant, r.PathValue("name"), r.PathValue("version"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, p, nil
}

// listSkills lists the tenant's skills, without their text.
func (s *Server) listSkills(ctx context.Context, c Caller, _ *http.Request) (int, any, error) {
	if s.Skills == nil {
		return http.StatusOK, map[string]any{"skills": []any{}}, nil
	}
	ps, err := s.Skills.List(ctx, c.Tenant)
	if err != nil {
		return 0, nil, err
	}
	views := make([]any, 0, len(ps))
	for _, p := range ps {
		views = append(views, skillView(p))
	}
	return http.StatusOK, map[string]any{"skills": views}, nil
}

// skillView is a skill as a list or a store answers: what it is, not its text.
func skillView(p skill.Package) map[string]any {
	files := make([]string, 0, len(p.Files))
	for path := range p.Files {
		files = append(files, path)
	}
	v := map[string]any{"name": p.Name, "version": p.Version, "description": p.Description}
	if len(files) > 0 {
		v["files"] = files
	}
	return v
}

// checkSkills refuses a definition naming a skill version the tenant doesn't
// have: a stored definition always builds (#101).
func (s *Server) checkSkills(ctx context.Context, tenant string, refs []skill.Ref) error {
	if len(refs) == 0 {
		return nil
	}
	if s.Skills == nil {
		return bad("this server keeps no skills, so an agent can't name any")
	}
	for _, ref := range refs {
		if _, err := s.Skills.Get(ctx, tenant, ref.Name, ref.Version); err != nil {
			if errors.Is(err, skill.ErrNotFound) {
				return bad("the agent names skill %s, which the tenant hasn't stored: PUT /skills/%s/%s first", ref, ref.Name, ref.Version)
			}
			return err
		}
	}
	return nil
}
