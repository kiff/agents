package server

import (
	"context"
	"net/http"

	"github.com/kiff/agents/session"
)

// skillsBody is what PUT /agents/{id}/sessions/{session}/skills takes: the
// skills to add to the session ("name@version"), each one the definition
// names.
type skillsBody struct {
	Add []string `json:"add"`
}

// addSessionSkills adds skills to a session mid-conversation (kiff-cloud#1487,
// the lighter Coach's way 2): the caller is the service that answers the
// agent's tools (as=service), for its own tenant; the session must be the
// agent's (404 otherwise, so another agent's or tenant's ids tell nothing);
// each skill is one the definition names, by name and version (400
// skill_not_named otherwise, nothing added). Adding one the session holds
// changes nothing. The next model turn carries the bodies after the
// instructions; no tool's answer ever does. Answers the skills the session
// holds, in order.
func (s *Server) addSessionSkills(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	if err := asService(r); err != nil {
		return 0, nil, err
	}
	var body skillsBody
	if err := decode(r, &body); err != nil {
		return 0, nil, err
	}
	if len(body.Add) == 0 || len(body.Add) > 8 {
		return 0, nil, bad("add names 1 to 8 skills, as name@version")
	}
	id, sid := r.PathValue("id"), r.PathValue("session")
	d, err := s.Agents.Get(ctx, c.Tenant, id)
	if err != nil {
		return 0, nil, err
	}
	sess, _, err := s.Sessions.Read(ctx, c.Tenant, sid)
	if err != nil {
		return 0, nil, err
	}
	if sess.Agent != id {
		return 0, nil, session.ErrNotFound
	}
	if err := d.CheckSessionSkills(body.Add); err != nil {
		return 0, nil, err
	}
	have, err := s.Sessions.AddSkills(ctx, c.Tenant, sid, body.Add)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"skills": have}, nil
}
