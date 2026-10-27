package attach

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"unicode/utf8"
)

// ErrMismatch: the bytes uploaded aren't what was declared.
var ErrMismatch = errors.New("attach: the file isn't what was declared")

// Verify holds data to what a was declared as: its size, its SHA-256 and
// its type, read from its first bytes rather than taken from the app. A PDF
// over MaxPDFPages is refused. For a Word or Excel file it returns the text
// the model reads instead; for any other, nil.
func Verify(a Attachment, data []byte) (text []byte, err error) {
	t, ok := Types[a.MediaType]
	if !ok {
		return nil, fmt.Errorf("%w: %s isn't a type that can be attached", ErrMismatch, a.MediaType)
	}
	if int64(len(data)) != a.Size {
		return nil, fmt.Errorf("%w: %d bytes, declared %d", ErrMismatch, len(data), a.Size)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != a.SHA256 {
		return nil, fmt.Errorf("%w: its SHA-256 differs", ErrMismatch)
	}
	if !sniff(a.MediaType, data) {
		return nil, fmt.Errorf("%w: its bytes aren't %s", ErrMismatch, a.MediaType)
	}
	switch t.Kind {
	case PDF:
		if n := pages(data); n > MaxPDFPages {
			return nil, fmt.Errorf("%s has %d pages; the most is %d", a.Name, n, MaxPDFPages)
		}
	case Word:
		return wordText(data)
	case Excel:
		return excelText(data)
	}
	return nil, nil
}

// sniff reports whether data's first bytes are mediaType's.
func sniff(mediaType string, data []byte) bool {
	switch mediaType {
	case "image/jpeg":
		return bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF})
	case "image/png":
		return bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n"))
	case "image/gif":
		return bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a"))
	case "image/webp":
		return len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP"))
	case "application/pdf":
		return bytes.HasPrefix(data, []byte("%PDF-"))
	case "text/plain", "text/csv", "text/markdown":
		return utf8.Valid(data) && bytes.IndexByte(data, 0) < 0
	default: // Word and Excel: a zip, opened only by extract
		return bytes.HasPrefix(data, []byte("PK\x03\x04"))
	}
}

// pageObject is a PDF page object's type: /Type /Page, not /Pages.
var pageObject = regexp.MustCompile(`/Type\s*/Page[^s]`)

// pages counts a PDF's page objects. Pages inside compressed object streams
// aren't seen, so it can undercount, never overcount: the model's own limit
// still applies to the rest.
func pages(data []byte) int { return len(pageObject.FindAllIndex(data, -1)) }
