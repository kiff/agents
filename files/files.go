// Package files is the files a taf delivers in a run: a report, a sheet or a
// document, kept 30 days for everyone on its account to open, save or share
// (kiff/tafs's Files). Unlike an attachment (package attach), which is one
// person's own message and only theirs to read, a delivered file belongs to
// the account: any member lists it, reads it and may delete it.
//
// A run delivers a file through the deliver_file tool, as text in v1 (CSV,
// Markdown, plain text, JSON). What it holds came from the run, which reads
// emails and sheets, so it's untrusted: its type is checked against its
// bytes, a CSV cell that would run as a formula is written as text, a
// preview is plain UTF-8 with no control or direction characters, its name
// is cleaned, and a download is served as an attachment of its checked type.
//
// Its record is in the agents table and its bytes in the attachments bucket,
// under their own prefix (Prefix), whose lifecycle rule expires them a day
// after the record's 30 days. Delete removes the bytes at once (the bucket
// keeps no versions) and the record with them.
package files

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ErrNotFound: no such file on the tenant's account.
var ErrNotFound = errors.New("files: not found")

// ErrInvalid: what a run tried to deliver can't be kept, saying why.
var ErrInvalid = errors.New("files: invalid")

// Type is a media type a run may deliver: the extension its name ends with,
// and the type a download is served as.
type Type struct {
	Ext    string
	Served string
}

// Types are what a run delivers in v1: text, which it writes itself. A PDF or
// an image comes with the workspace's own store, later.
var Types = map[string]Type{
	"text/csv":         {".csv", "text/csv; charset=utf-8"},
	"text/markdown":    {".md", "text/markdown; charset=utf-8"},
	"text/plain":       {".txt", "text/plain; charset=utf-8"},
	"application/json": {".json", "application/json; charset=utf-8"},
}

const mb = 1 << 20

// The limits: one file's size, a run's files, and an account's, past which
// its oldest files go first (Service.Deliver), so a taf in a loop never fills
// the bucket and a run is never refused for what others left.
const (
	MaxSize        = 10 * mb
	MaxPerRun      = 5
	MaxAccountSize = 500 * mb
	MaxAccountN    = 1000
)

// Kept is how long a file is kept, from its delivery.
const Kept = 30 * 24 * time.Hour

// ReadTTL is how long a download URL works.
const ReadTTL = time.Minute

// MaxName is the most characters a file's name has, its extension included.
const MaxName = 120

// MaxPreview is the most bytes of a file a preview shows.
const MaxPreview = 4 << 10

// File is one delivered file, as stored. Its bytes are in the blob store
// under Key.
type File struct {
	ID     string `json:"id"`
	Tenant string `json:"tenant"`
	// Agent is the taf whose run delivered it; Run and Session, that run and
	// its chat.
	Agent     string    `json:"taf"`
	Run       string    `json:"run"`
	Session   string    `json:"chat,omitempty"`
	Name      string    `json:"name"`
	MediaType string    `json:"media_type"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	Created   time.Time `json:"created"`
	Expires   time.Time `json:"expires"`
}

// Prefix is where delivered files' bytes are: apart from attachments, so the
// bucket's lifecycle rule for them is their own.
const Prefix = "delivered/"

// AgentPrefix is where an agent's delivered files are; with no agent, a
// tenant's.
func AgentPrefix(tenant, agent string) string {
	if agent == "" {
		return Prefix + tenant + "/"
	}
	return Prefix + tenant + "/" + agent + "/"
}

// Key is where its bytes are.
func (f File) Key() string { return AgentPrefix(f.Tenant, f.Agent) + f.ID }

// Filter narrows a list to one taf, one run or one chat; empty fields don't.
type Filter struct {
	Agent   string
	Run     string
	Session string
}

func (q Filter) keeps(f File) bool {
	return (q.Agent == "" || f.Agent == q.Agent) && (q.Run == "" || f.Run == q.Run) && (q.Session == "" || f.Session == q.Session)
}

// Store keeps files' records. Every read is by tenant, so one account never
// reaches another's file, even by a guessed id.
type Store interface {
	// Create stores a new file, due at its Expires.
	Create(ctx context.Context, f File) error
	// Get reads one; ErrNotFound when the tenant has none by that id.
	Get(ctx context.Context, tenant, id string) (File, error)
	// List lists the tenant's files, in no order: an account keeps at most
	// MaxAccountN.
	List(ctx context.Context, tenant string) ([]File, error)
	// Due lists files due by now, oldest first, at most limit.
	Due(ctx context.Context, now time.Time, limit int) ([]File, error)
	// Delete removes one record; a missing one is not an error.
	Delete(ctx context.Context, tenant, id string) error
}

// Blobs keeps files' bytes.
type Blobs interface {
	// Put writes data at key, stored as served (its checked type).
	Put(ctx context.Context, key, served string, data []byte) error
	// Get reads key, at most max bytes; more is an error.
	Get(ctx context.Context, key string, max int64) ([]byte, error)
	// PresignGetAs is a URL that reads key as a download named name, served
	// as served, until ttl passes.
	PresignGetAs(ctx context.Context, key, name, served string, ttl time.Duration) (string, error)
	// Delete removes keys; missing ones are not an error.
	Delete(ctx context.Context, keys ...string) error
	// DeletePrefix removes every key under prefix.
	DeletePrefix(ctx context.Context, prefix string) error
}

// Name is a file's name as kept and downloaded: its base only, with no
// control, format or direction characters (a U+202E that would show
// "invoice‮fdp.exe" as "invoiceexe.pdf"), at most MaxName characters, and
// ending in its type's extension, so nothing downloads under another type's
// name. It's "file" plus the extension when nothing is left.
func Name(n, mediaType string) string {
	ext := Types[mediaType].Ext
	n = path.Base(strings.ReplaceAll(n, "\\", "/"))
	n = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, n)
	n = strings.Trim(n, " .")
	if strings.EqualFold(path.Ext(n), ext) {
		n = n[:len(n)-len(path.Ext(n))]
	}
	if n == "" {
		n = "file"
	}
	if r := []rune(n); len(r) > MaxName-len(ext) {
		n = strings.TrimRight(string(r[:MaxName-len(ext)]), " .")
	}
	return n + ext
}

// Content checks what a run delivers against its type and returns the bytes
// to keep: valid UTF-8 with no NUL, JSON that parses, and CSV that parses,
// with each cell that would run as a formula written as text (Defused).
func Content(mediaType string, data []byte) ([]byte, error) {
	if _, ok := Types[mediaType]; !ok {
		return nil, fmt.Errorf("%w: %q can't be delivered: CSV, Markdown, plain text and JSON can", ErrInvalid, mediaType)
	}
	switch {
	case len(data) == 0:
		return nil, fmt.Errorf("%w: the file is empty", ErrInvalid)
	case len(data) > MaxSize:
		return nil, fmt.Errorf("%w: the file is %d bytes; the most is %d (10 MB)", ErrInvalid, len(data), MaxSize)
	case !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0:
		return nil, fmt.Errorf("%w: a %s file is UTF-8 text", ErrInvalid, mediaType)
	}
	switch mediaType {
	case "application/json":
		if !json.Valid(data) {
			return nil, fmt.Errorf("%w: it isn't valid JSON", ErrInvalid)
		}
	case "text/csv":
		out, err := Defused(data)
		if err != nil {
			return nil, fmt.Errorf("%w: it isn't valid CSV: %v", ErrInvalid, err)
		}
		if len(out) > MaxSize {
			return nil, fmt.Errorf("%w: the file is over 10 MB once its formula cells are written as text", ErrInvalid)
		}
		return out, nil
	}
	return data, nil
}

// Defused is a CSV with every cell a spreadsheet would run as a formula
// written as text: one starting with =, +, -, @, a tab or a carriage return
// gets a ' before it, so "=HYPERLINK(…)" opens as those words. A number
// ("-12.50", "+33") is left as it is.
func Defused(data []byte) ([]byte, error) {
	r := csv.NewReader(bytes.NewReader(data))
	// Strict quoting: a bare quote would make the reader guess where a cell
	// ends, and a cell it guessed wrong could hide a formula.
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	for _, rec := range records {
		for i, cell := range rec {
			if formula(cell) {
				rec[i] = "'" + cell
			}
		}
	}
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	if err := w.WriteAll(records); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func formula(cell string) bool {
	if cell == "" || !strings.ContainsRune("=+-@\t\r", rune(cell[0])) {
		return false
	}
	if cell[0] == '+' || cell[0] == '-' {
		_, err := strconv.ParseFloat(strings.ReplaceAll(cell[1:], ",", ""), 64)
		return err != nil
	}
	return true
}

// Preview is the start of a file as plain text, for the apps to draw as text
// and never as Markdown or HTML: at most MaxPreview bytes, cut on a whole
// character, with no control characters but line breaks and tabs, and no
// format or direction characters.
func Preview(data []byte) string {
	if len(data) > MaxPreview {
		data = data[:MaxPreview]
		for len(data) > 0 && !utf8.Valid(data) {
			data = data[:len(data)-1]
		}
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError:
			return -1
		}
		return r
	}, string(data))
}
