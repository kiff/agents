// Package attach is the files a person attaches to a chat message
// (kiff/tafs#209): a photo, a PDF, text, or a Word or Excel file read as
// text. A file is the person's own message, in their own chat. It lives as
// long as that message, is sent to the model inline with it, and is never
// kept for the agent, in its memory, or anywhere a door reaches.
//
// A file arrives in three steps. Create records what the app says it will
// upload and returns a short-lived upload URL for exactly that size, type and
// checksum, so the bytes go straight to the blob store, never through the
// API. Check, when the message that names it is sent, reads the bytes back
// and holds them to what was declared and to their real type (their first
// bytes), and turns a Word or Excel file into text. Bind ties it to that
// message's session and run: a file is bound once, and only to its uploader's
// own message.
//
// What a file says is context, not authority: it reaches the model fenced, as
// the person's material to read (FileNote), and nothing in it changes a Card,
// a held call or a tool.
package attach

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound: no such attachment for the tenant, or not the caller's.
var ErrNotFound = errors.New("attach: not found")

// ErrBound: the attachment is already part of a message.
var ErrBound = errors.New("attach: already sent")

// Kind is how the model reads a file.
type Kind string

const (
	Image Kind = "image" // a photo or screenshot, as an image
	PDF   Kind = "pdf"   // in full: text, tables and charts
	Text  Kind = "text"  // plain text, CSV or Markdown, as text
	Word  Kind = "word"  // .docx, its paragraphs and tables as text
	Excel Kind = "excel" // .xlsx, each sheet as CSV
)

// Type is an accepted media type: its kind and the most a file of it may
// weigh, in bytes.
type Type struct {
	Kind Kind
	Max  int64
}

const mb = 1 << 20

// Types are the media types a person may attach. A Word or Excel file with
// macros (.docm, .xlsm) has its own media type, and isn't here.
var Types = map[string]Type{
	"image/jpeg":      {Image, 10 * mb},
	"image/png":       {Image, 10 * mb},
	"image/gif":       {Image, 10 * mb},
	"image/webp":      {Image, 10 * mb},
	"application/pdf": {PDF, 25 * mb},
	"text/plain":      {Text, 2 * mb},
	"text/csv":        {Text, 2 * mb},
	"text/markdown":   {Text, 2 * mb},
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": {Word, 2 * mb},
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":       {Excel, 2 * mb},
}

// MaxPerMessage is how many files one message may carry.
const MaxPerMessage = 5

// MaxPDFPages is the most pages a PDF may have: the most the model reads.
const MaxPDFPages = 100

// MaxText is the most text a Word or Excel file may hold once extracted.
const MaxText = 2 * mb

// TurnBudget is how many bytes of files one model turn carries, newest first:
// under the model's request limit once encoded. TurnTokens bounds what they
// take of the model's context, estimated (Tokens). An older file past either
// reaches the model as a line saying it's no longer in view.
const (
	TurnBudget = 20 * mb
	TurnTokens = 100_000
	// EarlierTokens is the share of TurnTokens for files sent with the
	// chat's earlier messages: each run of the chat sends them again, so a
	// long PDF from an hour ago doesn't cost its full size on every message.
	EarlierTokens = 30_000
)

// Tokens estimates what a file's bytes take of the model's context: a photo
// about 1,600 tokens (the model scales it), a PDF about 2,000 a page (text and
// the page's image), text about one per four bytes.
func Tokens(mediaType string, data []byte) int {
	switch t := Types[mediaType]; {
	case t.Kind == Image:
		return 1600
	case t.Kind == PDF:
		return 2000 * max(pages(data), 1+len(data)/(100<<10))
	}
	return len(data)/4 + 1
}

// UploadTTL is how long an upload URL works; ReadTTL, a download URL.
const (
	UploadTTL = 5 * time.Minute
	ReadTTL   = time.Minute
)

// Abandoned is how long an upload no message names is kept.
const Abandoned = time.Hour

// Attachment is one file, as stored. Its bytes are in the blob store under
// Key; a Word or Excel file's text under TextKey.
type Attachment struct {
	ID     string `json:"id"`
	Tenant string `json:"tenant"`
	Agent  string `json:"agent"`
	// Owner is who attached it, as run.Requester's Key: only they send it,
	// read it or see it listed.
	Owner     string    `json:"owner"`
	MediaType string    `json:"media_type"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"` // hex
	Created   time.Time `json:"created"`
	// Checked: its bytes matched Size, SHA256 and their type, and a Word or
	// Excel file's text is stored.
	Checked bool `json:"checked,omitempty"`
	// Session and Run are the message it was sent with, once bound; Expires
	// is when it goes with that message (zero: as long as its session).
	Session string    `json:"session,omitempty"`
	Run     string    `json:"run,omitempty"`
	Bound   time.Time `json:"bound,omitzero"`
	Expires time.Time `json:"expires,omitzero"`
}

// Kind is how the model reads it.
func (a Attachment) Kind() Kind { return Types[a.MediaType].Kind }

// Key is where its bytes are in the blob store: under its tenant and agent,
// so removing either removes its files by prefix.
func (a Attachment) Key() string { return Prefix(a.Tenant, a.Agent) + a.ID }

// TextKey is where a Word or Excel file's text is.
func (a Attachment) TextKey() string { return a.Key() + ".txt" }

// Prefix is where an agent's files are in the blob store; with no agent, a
// tenant's.
func Prefix(tenant, agent string) string {
	if agent == "" {
		return tenant + "/"
	}
	return tenant + "/" + agent + "/"
}

// Due is when the attachment goes: an hour after it was made while no
// message names it, then with its message.
func (a Attachment) Due() time.Time {
	if a.Session == "" {
		return a.Created.Add(Abandoned)
	}
	return a.Expires
}

// Ref is an attachment as a message records it: enough to show it and to
// load it again, never its bytes.
type Ref struct {
	ID        string `json:"id"`
	Kind      Kind   `json:"kind"`
	MediaType string `json:"media_type"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
}

// Ref is the attachment as a message records it.
func (a Attachment) Ref() Ref {
	return Ref{ID: a.ID, Kind: a.Kind(), MediaType: a.MediaType, Name: a.Name, Size: a.Size}
}

// Store keeps attachments' records. Every read is by tenant, so one account
// never reaches another's file, even by a guessed id.
type Store interface {
	// Create stores a new attachment, and makes it due at Due.
	Create(ctx context.Context, a Attachment) error
	// Get reads one; ErrNotFound when the tenant has none by that id.
	Get(ctx context.Context, tenant, id string) (Attachment, error)
	// Check marks it Checked.
	Check(ctx context.Context, tenant, id string) error
	// Bind ties an unbound attachment to session and run, due at expires
	// (zero: with its session). ErrBound when a message already has it.
	Bind(ctx context.Context, tenant, id, session, run string, at, expires time.Time) error
	// Session lists a session's attachments; Agent, an agent's; Tenant, a
	// tenant's. Each for removing them.
	Session(ctx context.Context, tenant, session string) ([]Attachment, error)
	Agent(ctx context.Context, tenant, agent string) ([]Attachment, error)
	// Due lists attachments due by now, oldest first, at most limit.
	Due(ctx context.Context, now time.Time, limit int) ([]Attachment, error)
	// Delete removes one record; a missing one is not an error.
	Delete(ctx context.Context, tenant, id string) error
}

// Upload is how the app sends a file's bytes: a PUT to URL with Headers.
type Upload struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Expires time.Time         `json:"expires"`
}

// Blobs keeps files' bytes.
type Blobs interface {
	// PresignPut is a URL that takes exactly size bytes of mediaType with
	// that SHA-256 (hex) at key, until ttl passes.
	PresignPut(ctx context.Context, key, mediaType string, size int64, sha256 string, ttl time.Duration) (Upload, error)
	// PresignGet is a URL that reads key, as a download named name, until
	// ttl passes.
	PresignGet(ctx context.Context, key, name string, ttl time.Duration) (string, error)
	// Get reads key, at most max bytes; more is an error.
	Get(ctx context.Context, key string, max int64) ([]byte, error)
	// Put writes data at key.
	Put(ctx context.Context, key, mediaType string, data []byte) error
	// Delete removes keys; missing ones are not an error.
	Delete(ctx context.Context, keys ...string) error
	// DeletePrefix removes every key under prefix.
	DeletePrefix(ctx context.Context, prefix string) error
}

// TooBig is a file over its limit, saying which.
type TooBig struct {
	Name  string
	Size  int64
	Limit int64
}

func (e TooBig) Error() string {
	return fmt.Sprintf("%s is %s; the most is %s", e.Name, human(e.Size), human(e.Limit))
}

// human is a size as a person reads it.
func human(n int64) string {
	switch {
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/mb)
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}
