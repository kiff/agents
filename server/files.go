package server

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/kiff/agents/files"
	"github.com/kiff/agents/run"
)

// fileView is a delivered file as the API shows it to a member of its
// account: never its bytes, which a download URL reads.
type fileView struct {
	ID        string    `json:"id"`
	Taf       string    `json:"taf"`
	Run       string    `json:"run"`
	Chat      string    `json:"chat,omitempty"`
	Name      string    `json:"name"`
	MediaType string    `json:"media_type"`
	Size      int64     `json:"size"`
	Created   time.Time `json:"created"`
	Expires   time.Time `json:"expires"`
}

func viewFile(f files.File) fileView {
	return fileView{ID: f.ID, Taf: f.Agent, Run: f.Run, Chat: f.Session, Name: f.Name, MediaType: f.MediaType,
		Size: f.Size, Created: f.Created, Expires: f.Expires}
}

// member is the by parameter: the member of the account reading or deleting a
// file, as tafs cloud checked them, so the log says who. A file is the
// account's: any member may, which tafs cloud decides.
func member(r *http.Request) (string, error) {
	by := r.URL.Query().Get("by")
	kind, id, _ := strings.Cut(by, ":")
	if q := (run.Requester{Kind: run.RequesterKind(kind), ID: id}); q.Kind != run.ByMember || !q.Valid() {
		return "", bad("name the member reading or deleting the file: by=member:<subject>")
	}
	return by, nil
}

// filesPage is how many files one list answers.
const filesPage = 100

// listFiles is GET /files?taf=&run=&chat=&after=: the caller's account's
// delivered files, newest first, a page at a time (after: the last id of the
// page before), narrowed to one taf, run or chat. A taf, run or chat of
// another account lists nothing: every read is in the caller's tenant.
func (s *Server) listFiles(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	if s.Delivered == nil {
		return 0, nil, unavailable("this server keeps no delivered files")
	}
	q := r.URL.Query()
	all, err := s.Delivered.List(ctx, c.Tenant, files.Filter{Agent: q.Get("taf"), Run: q.Get("run"), Session: q.Get("chat")})
	if err != nil {
		return 0, nil, err
	}
	if after := q.Get("after"); after != "" {
		i := slices.IndexFunc(all, func(f files.File) bool { return f.ID == after })
		if i < 0 {
			return 0, nil, bad("after names no file in this list")
		}
		all = all[i+1:]
	}
	out := struct {
		Files []fileView `json:"files"`
		Next  string     `json:"next,omitempty"`
	}{Files: []fileView{}}
	for i, f := range all {
		if i == filesPage {
			out.Next = all[i-1].ID
			break
		}
		out.Files = append(out.Files, viewFile(f))
	}
	return http.StatusOK, out, nil
}

// readFile is GET /files/{file}?by=member:<subject>: one file of the caller's
// account, and the start of it as plain text (files.Preview), which the apps
// draw as text, never as Markdown or HTML.
func (s *Server) readFile(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	if s.Delivered == nil {
		return 0, nil, unavailable("this server keeps no delivered files")
	}
	by, err := member(r)
	if err != nil {
		return 0, nil, err
	}
	f, preview, err := s.Delivered.Read(ctx, c.Tenant, r.PathValue("file"), by)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, struct {
		fileView
		Preview   string `json:"preview"`
		Truncated bool   `json:"preview_truncated"`
	}{viewFile(f), preview, f.Size > files.MaxPreview}, nil
}

// downloadFile is GET /files/{file}/download?by=member:<subject>: a URL to
// the file's bytes that works for a minute, served as an attachment of its
// checked type under its cleaned name. The app reads the bytes from it, to
// save or share the file itself: the URL is never given out.
func (s *Server) downloadFile(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	if s.Delivered == nil {
		return 0, nil, unavailable("this server keeps no delivered files")
	}
	by, err := member(r)
	if err != nil {
		return 0, nil, err
	}
	f, url, until, err := s.Delivered.DownloadUntil(ctx, c.Tenant, r.PathValue("file"), by)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, struct {
		fileView
		URL     string    `json:"url"`
		Expires time.Time `json:"url_expires"`
	}{viewFile(f), url, until}, nil
}

// deleteFile is DELETE /files/{file}?by=member:<subject>: the file goes for
// everyone on the account, its bytes at once and for good, and the log names
// the member who deleted it.
func (s *Server) deleteFile(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	if s.Delivered == nil {
		return 0, nil, unavailable("this server keeps no delivered files")
	}
	by, err := member(r)
	if err != nil {
		return 0, nil, err
	}
	if err := s.Delivered.Delete(ctx, c.Tenant, r.PathValue("file"), by); err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}
