package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/kiff/agents/attach"
	"github.com/kiff/agents/run"
)

// attachmentView is an attachment as the API shows it to the person who
// attached it.
type attachmentView struct {
	attach.Ref
	Created time.Time `json:"created"`
	Sent    bool      `json:"sent"`
}

func viewAttachment(a attach.Attachment) attachmentView {
	return attachmentView{Ref: a.Ref(), Created: a.Created, Sent: a.Session != ""}
}

// person is the owner parameter, which these routes need: a person, never
// the service, since a file is only ever its sender's.
func person(r *http.Request) (string, error) {
	owner := r.URL.Query().Get("owner")
	kind, id, _ := strings.Cut(owner, ":")
	if q := (run.Requester{Kind: run.RequesterKind(kind), ID: id}); q.Kind != run.ByMember || !q.Valid() {
		return "", bad("name the person attaching or reading the file: owner=member:<subject>")
	}
	return owner, nil
}

// createAttachment is POST /agents/{id}/attachments?owner=member:<subject>
// (kiff/tafs#209): the person declares a file they'll attach to a message in
// their chat with the agent ({media_type, name, size, sha256}), and gets an
// upload for exactly that: a PUT to a URL, with headers to send as given,
// working for a few minutes. A type that can't be attached, or a file over
// its limit, is refused here, before any upload, saying which limit.
func (s *Server) createAttachment(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	if s.Files == nil {
		return 0, nil, unavailable("this server keeps no attachments")
	}
	owner, err := person(r)
	if err != nil {
		return 0, nil, err
	}
	id := r.PathValue("id")
	if _, err := s.Agents.Get(ctx, c.Tenant, id); err != nil {
		return 0, nil, err
	}
	if err := s.mayRead(ctx, c.Tenant, id, owner); err != nil {
		return 0, nil, err
	}
	var d attach.Declared
	if err := decode(r, &d); err != nil {
		return 0, nil, err
	}
	a, up, err := s.Files.Create(ctx, c.Tenant, id, owner, d)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, struct {
		Attachment attachmentView `json:"attachment"`
		Upload     attach.Upload  `json:"upload"`
	}{viewAttachment(a), up}, nil
}

// readAttachment is GET /agents/{id}/attachments/{att}?owner=member:<subject>:
// the file, and a download URL that works for a minute, for the person who
// attached it and no one else: not the service, an admin or the account's
// owner, since no one reads another person's chat.
func (s *Server) readAttachment(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	if s.Files == nil {
		return 0, nil, unavailable("this server keeps no attachments")
	}
	owner, err := person(r)
	if err != nil {
		return 0, nil, err
	}
	id := r.PathValue("id")
	if err := s.mayRead(ctx, c.Tenant, id, owner); err != nil {
		return 0, nil, err
	}
	a, url, err := s.Files.Read(ctx, c.Tenant, id, owner, r.PathValue("att"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, struct {
		attachmentView
		URL     string    `json:"url"`
		Expires time.Time `json:"url_expires"`
	}{viewAttachment(a), url, time.Now().Add(attach.ReadTTL)}, nil
}
