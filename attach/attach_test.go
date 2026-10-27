package attach_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/attach"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/store/memory"
)

const (
	docx = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	xlsx = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func declared(mt, name string, b []byte) attach.Attachment {
	return attach.Attachment{ID: "att_1", Tenant: "t1", Agent: "a1", Owner: "member:ana", MediaType: mt, Name: name, Size: int64(len(b)), SHA256: sum(b)}
}

// zipOf is a zip of name→content, each deflated.
func zipOf(t *testing.T, parts map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range parts {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const wordDoc = `<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>
<w:p><w:r><w:t>Refund policy</w:t></w:r></w:p>
<w:p><w:r><w:t xml:space="preserve">Refunds up to </w:t></w:r><w:r><w:t>30 days.</w:t></w:r></w:p>
<w:tbl><w:tr><w:tc><w:p><w:r><w:t>Item</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>Days</w:t></w:r></w:p></w:tc></w:tr>
<w:tr><w:tc><w:p><w:r><w:t>Shoes</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>60</w:t></w:r></w:p></w:tc></w:tr></w:tbl>
</w:body></w:document>`

func TestVerifyReadsTheType(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 20)...)
	if text, err := attach.Verify(declared("image/png", "a.png", png), png); err != nil || text != nil {
		t.Fatalf("a PNG: %q, %v", text, err)
	}
	for name, c := range map[string]struct {
		mt   string
		data []byte
	}{
		"a script called a PNG":     {"image/png", []byte("<script>alert(1)</script>")},
		"an HTML file called a PDF": {"application/pdf", []byte("<html></html>")},
		"binary called text":        {"text/plain", []byte("ok\x00\x01")},
		"a JPEG called a WebP":      {"image/webp", []byte("\xFF\xD8\xFF\xE0rest")},
	} {
		if _, err := attach.Verify(declared(c.mt, "f", c.data), c.data); !errors.Is(err, attach.ErrMismatch) {
			t.Errorf("%s: %v", name, err)
		}
	}
	a := declared("image/png", "a.png", png)
	a.Size++
	if _, err := attach.Verify(a, png); !errors.Is(err, attach.ErrMismatch) {
		t.Errorf("another size: %v", err)
	}
	a = declared("image/png", "a.png", png)
	a.SHA256 = sum([]byte("other"))
	if _, err := attach.Verify(a, png); !errors.Is(err, attach.ErrMismatch) {
		t.Errorf("another checksum: %v", err)
	}
}

func TestVerifyCountsPDFPages(t *testing.T) {
	pdf := []byte("%PDF-1.7\n" + strings.Repeat("1 0 obj << /Type /Page >> endobj\n", attach.MaxPDFPages) + "<< /Type /Pages >>")
	if _, err := attach.Verify(declared("application/pdf", "a.pdf", pdf), pdf); err != nil {
		t.Fatalf("%d pages: %v", attach.MaxPDFPages, err)
	}
	pdf = []byte("%PDF-1.7\n" + strings.Repeat("<< /Type /Page >>\n", attach.MaxPDFPages+1))
	if _, err := attach.Verify(declared("application/pdf", "a.pdf", pdf), pdf); err == nil || !strings.Contains(err.Error(), "pages") {
		t.Fatalf("%d pages: %v", attach.MaxPDFPages+1, err)
	}
}

func TestWordIsReadAsText(t *testing.T) {
	data := zipOf(t, map[string]string{"word/document.xml": wordDoc, "[Content_Types].xml": "<Types/>"})
	text, err := attach.Verify(declared(docx, "policy.docx", data), data)
	if err != nil {
		t.Fatal(err)
	}
	want := "Refund policy\nRefunds up to 30 days.\nItem \tDays \t\nShoes \t60"
	if string(text) != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
}

func TestExcelIsReadAsCSV(t *testing.T) {
	data := zipOf(t, map[string]string{
		"xl/workbook.xml":            `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Orders" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="worksheet" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/sharedStrings.xml":       `<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><si><t>Order</t></si><si><t>Total, EUR</t></si><si><t>T-1001</t></si></sst>`,
		"xl/worksheets/sheet1.xml": `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>
<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row>
<row r="2"><c r="A2" t="s"><v>2</v></c><c r="C2"><v>40</v></c></row></sheetData></worksheet>`,
	})
	text, err := attach.Verify(declared(xlsx, "orders.xlsx", data), data)
	if err != nil {
		t.Fatal(err)
	}
	want := "Sheet: Orders\nOrder,\"Total, EUR\"\nT-1001,,40"
	if string(text) != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
}

func TestOfficeFilesAreOpenedSafely(t *testing.T) {
	// A zip bomb: one part that unpacks far past its limit.
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, _ := w.Create("word/document.xml")
	zeros := make([]byte, 1<<20)
	for range 20 {
		_, _ = f.Write(zeros)
	}
	_ = w.Close()
	bomb := buf.Bytes()
	if _, err := attach.Verify(declared(docx, "bomb.docx", bomb), bomb); !errors.Is(err, attach.ErrUnsafe) {
		t.Errorf("a zip bomb: %v", err)
	}

	// An external entity: refused, never expanded or fetched.
	xxe := zipOf(t, map[string]string{"word/document.xml": `<?xml version="1.0"?>
<!DOCTYPE d [<!ENTITY x SYSTEM "file:///etc/passwd">]>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>&x;</w:t></w:r></w:p></w:body></w:document>`})
	text, err := attach.Verify(declared(docx, "xxe.docx", xxe), xxe)
	if !errors.Is(err, attach.ErrUnsafe) || strings.Contains(string(text), "root:") {
		t.Errorf("an external entity: %q, %v", text, err)
	}

	// Macros: a Word file carrying a VBA project.
	macro := zipOf(t, map[string]string{"word/document.xml": wordDoc, "word/vbaProject.bin": "x"})
	if _, err := attach.Verify(declared(docx, "m.docx", macro), macro); !errors.Is(err, attach.ErrUnsafe) {
		t.Errorf("macros: %v", err)
	}

	// Too many parts.
	many := map[string]string{"word/document.xml": wordDoc}
	for i := range 1001 {
		many["x/"+strings.Repeat("a", 3)+string(rune('a'+i%26))+time.Duration(i).String()] = ""
	}
	if data := zipOf(t, many); true {
		if _, err := attach.Verify(declared(docx, "many.docx", data), data); !errors.Is(err, attach.ErrUnsafe) {
			t.Errorf("1002 parts: %v", err)
		}
	}
}

func service() (*attach.Service, *memory.Blobs, *time.Time) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	blobs := memory.NewBlobs()
	return &attach.Service{Store: memory.NewAttachments(), Blobs: blobs, Now: func() time.Time { return now }}, blobs, &now
}

func TestCreateRefusesBeforeUpload(t *testing.T) {
	s, _, _ := service()
	ctx := context.Background()
	for name, c := range map[string]struct {
		owner string
		d     attach.Declared
	}{
		"no person":       {"", attach.Declared{MediaType: "image/png", Name: "a.png", Size: 10, SHA256: sum(nil)}},
		"an executable":   {"member:ana", attach.Declared{MediaType: "application/x-msdownload", Name: "a.exe", Size: 10, SHA256: sum(nil)}},
		"macros":          {"member:ana", attach.Declared{MediaType: "application/vnd.ms-word.document.macroEnabled.12", Name: "a.docm", Size: 10, SHA256: sum(nil)}},
		"a photo too big": {"member:ana", attach.Declared{MediaType: "image/jpeg", Name: "big.jpg", Size: 11 << 20, SHA256: sum(nil)}},
		"no checksum":     {"member:ana", attach.Declared{MediaType: "image/png", Name: "a.png", Size: 10}},
	} {
		if _, _, err := s.Create(ctx, "t1", "a1", c.owner, c.d); !errors.Is(err, attach.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	_, _, err := s.Create(ctx, "t1", "a1", "member:ana", attach.Declared{MediaType: "image/jpeg", Name: "big.jpg", Size: 11 << 20, SHA256: sum(nil)})
	if !strings.Contains(err.Error(), "big.jpg is 11.0 MB; the most is 10.0 MB") {
		t.Errorf("the limit isn't said: %v", err)
	}
}

// upload creates an attachment and puts its bytes, as the app would.
func upload(t *testing.T, s *attach.Service, blobs *memory.Blobs, owner, mt, name string, data []byte) attach.Attachment {
	t.Helper()
	a, up, err := s.Create(context.Background(), "t1", "a1", owner, attach.Declared{MediaType: mt, Name: name, Size: int64(len(data)), SHA256: sum(data)})
	if err != nil {
		t.Fatal(err)
	}
	if up.Method != "PUT" || !strings.HasSuffix(up.URL, a.Key()) {
		t.Fatalf("upload = %+v", up)
	}
	if err := blobs.Put(context.Background(), a.Key(), mt, data); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestTakeBindLoad(t *testing.T) {
	s, blobs, now := service()
	ctx := context.Background()
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 20)...)
	word := zipOf(t, map[string]string{"word/document.xml": wordDoc})
	a := upload(t, s, blobs, "member:ana", "image/png", "../../receipt.png", png)
	b := upload(t, s, blobs, "member:ana", docx, "policy.docx", word)
	if a.Name != "receipt.png" {
		t.Fatalf("name = %q", a.Name)
	}

	// Another person, another agent, another tenant: not theirs.
	if _, err := s.Take(ctx, "t1", "a1", "member:bo", []string{a.ID}); !errors.Is(err, attach.ErrNotFound) {
		t.Fatalf("another person took it: %v", err)
	}
	if _, err := s.Take(ctx, "t1", "a2", "member:ana", []string{a.ID}); !errors.Is(err, attach.ErrNotFound) {
		t.Fatalf("another agent took it: %v", err)
	}
	if _, err := s.Take(ctx, "t2", "a1", "member:ana", []string{a.ID}); !errors.Is(err, attach.ErrNotFound) {
		t.Fatalf("another tenant took it: %v", err)
	}
	if _, err := s.Take(ctx, "t1", "a1", "member:ana", []string{a.ID, a.ID}); !errors.Is(err, attach.ErrInvalid) {
		t.Fatalf("named twice: %v", err)
	}

	as, err := s.Take(ctx, "t1", "a1", "member:ana", []string{a.ID, b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Bind(ctx, as, "s1", "r1", now.Add(30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Take(ctx, "t1", "a1", "member:ana", []string{a.ID}); !errors.Is(err, attach.ErrBound) {
		t.Fatalf("sent twice: %v", err)
	}

	refs := []attach.Ref{as[0].Ref(), as[1].Ref()}
	msgs := []model.Message{{Role: model.User, Blocks: append([]model.Block{{Kind: model.Text, Text: "Is this refundable?"}}, attach.Blocks(refs)...)}}
	if msgs[0].Blocks[1].Text != attach.FileNote {
		t.Fatalf("no fence: %+v", msgs[0].Blocks)
	}
	if err := s.Load(ctx, "t1", msgs); err != nil {
		t.Fatal(err)
	}
	img, doc := msgs[0].Blocks[2], msgs[0].Blocks[3]
	if img.MediaType != "image/png" || !bytes.Equal(img.Bytes, png) {
		t.Fatalf("image = %+v", img)
	}
	if doc.MediaType != model.FileText || !strings.HasPrefix(string(doc.Bytes), "Refund policy") {
		t.Fatalf("document = %q (%s)", doc.Bytes, doc.MediaType)
	}

	// Only its owner reads it.
	if _, _, err := s.Read(ctx, "t1", "a1", "", a.ID); !errors.Is(err, attach.ErrNotFound) {
		t.Fatalf("the service read it: %v", err)
	}
	if _, _, err := s.Read(ctx, "t1", "a1", "member:bo", a.ID); !errors.Is(err, attach.ErrNotFound) {
		t.Fatalf("another person read it: %v", err)
	}
	if _, url, err := s.Read(ctx, "t1", "a1", "member:ana", a.ID); err != nil || url == "" {
		t.Fatalf("its owner: %q, %v", url, err)
	}

	// It goes with its message.
	*now = now.Add(30*24*time.Hour + time.Minute)
	if n, err := s.Sweep(ctx, 10); err != nil || n != 2 {
		t.Fatalf("Sweep = %d, %v", n, err)
	}
	if keys := blobs.Keys(); len(keys) != 0 {
		t.Fatalf("left: %v", keys)
	}
}

func TestTakeDeletesAMismatch(t *testing.T) {
	s, blobs, _ := service()
	ctx := context.Background()
	a, _, err := s.Create(ctx, "t1", "a1", "member:ana", attach.Declared{MediaType: "image/png", Name: "a.png", Size: 5, SHA256: sum([]byte("hello"))})
	if err != nil {
		t.Fatal(err)
	}
	_ = blobs.Put(ctx, a.Key(), "image/png", []byte("hello")) // the right size and checksum, not a PNG
	if _, err := s.Take(ctx, "t1", "a1", "member:ana", []string{a.ID}); !errors.Is(err, attach.ErrInvalid) {
		t.Fatalf("Take = %v", err)
	}
	if _, err := s.Store.Get(ctx, "t1", a.ID); !errors.Is(err, attach.ErrNotFound) || len(blobs.Keys()) != 0 {
		t.Fatalf("kept: %v %v", err, blobs.Keys())
	}
}

func TestAnUnsentUploadGoesAfterAnHour(t *testing.T) {
	s, blobs, now := service()
	ctx := context.Background()
	upload(t, s, blobs, "member:ana", "text/plain", "notes.txt", []byte("hello"))
	*now = now.Add(59 * time.Minute)
	if n, _ := s.Sweep(ctx, 10); n != 0 {
		t.Fatalf("swept early: %d", n)
	}
	*now = now.Add(2 * time.Minute)
	if n, _ := s.Sweep(ctx, 10); n != 1 || len(blobs.Keys()) != 0 {
		t.Fatalf("swept %d, left %v", n, blobs.Keys())
	}
}

func TestLoadKeepsToTheBudget(t *testing.T) {
	s, blobs, _ := service()
	ctx := context.Background()
	big := func(n byte) []byte { return append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{n}, 9<<20)...) }
	var refs [][]attach.Ref
	for i := range 3 { // 9 MB each: the newest two fit in 20 MB
		a := upload(t, s, blobs, "member:ana", "image/png", "photo.png", big(byte('a'+i)))
		as, err := s.Take(ctx, "t1", "a1", "member:ana", []string{a.ID})
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Bind(ctx, as, "s1", "r"+string(rune('1'+i)), time.Time{})
		refs = append(refs, []attach.Ref{as[0].Ref()})
	}
	var msgs []model.Message
	for _, r := range refs {
		msgs = append(msgs, model.Message{Role: model.User, Blocks: attach.Blocks(r)}, model.Message{Role: model.Assistant, Blocks: []model.Block{{Kind: model.Text, Text: "ok"}}})
	}
	if err := s.Load(ctx, "t1", msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs[0].Blocks[1].Bytes) != 0 || len(msgs[2].Blocks[1].Bytes) == 0 || len(msgs[4].Blocks[1].Bytes) == 0 {
		t.Fatal("the oldest should be out of view, the newest two in it")
	}
}

// A long PDF fills the context long before 20 MB: the newest that fit the
// token budget are read, the rest are out of view. A file from an earlier
// message gets a smaller share than the one being answered.
func TestLoadKeepsToTheTokenBudget(t *testing.T) {
	s, blobs, _ := service()
	ctx := context.Background()
	pdf := []byte("%PDF-1.7\n" + strings.Repeat("<< /Type /Page >>\n", 40)) // ~80,000 tokens
	var msgs []model.Message
	for i := range 2 {
		a := upload(t, s, blobs, "member:ana", "application/pdf", "long.pdf", append(pdf, byte('0'+i)))
		as, err := s.Take(ctx, "t1", "a1", "member:ana", []string{a.ID})
		if err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, model.Message{Role: model.User, Blocks: attach.Blocks([]attach.Ref{as[0].Ref()})})
	}
	if err := s.Load(ctx, "t1", msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs[0].Blocks[1].Bytes) != 0 || len(msgs[1].Blocks[1].Bytes) == 0 {
		t.Fatal("two 40-page PDFs: only the newest should be in view")
	}
}

func TestEarlierFilesTakeASmallerShare(t *testing.T) {
	s, blobs, _ := service()
	ctx := context.Background()
	pdf := []byte("%PDF-1.7\n" + strings.Repeat("<< /Type /Page >>\n", 20)) // ~40,000 tokens
	var msgs []model.Message
	for i := range 2 {
		a := upload(t, s, blobs, "member:ana", "application/pdf", "doc.pdf", append(pdf, byte('0'+i)))
		as, err := s.Take(ctx, "t1", "a1", "member:ana", []string{a.ID})
		if err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, model.Message{Role: model.User, Blocks: attach.Blocks([]attach.Ref{as[0].Ref()})},
			model.Message{Role: model.Assistant, Blocks: []model.Block{{Kind: model.Text, Text: "read it"}}})
	}
	msgs = msgs[:3] // the second PDF is the message being answered
	if err := s.Load(ctx, "t1", msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs[2].Blocks[1].Bytes) == 0 {
		t.Fatal("the file being answered should be in view")
	}
	if len(msgs[0].Blocks[1].Bytes) != 0 {
		t.Fatal("an earlier 20-page PDF is over the earlier messages' share: it should be out of view")
	}
}
