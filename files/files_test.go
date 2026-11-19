package files_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kiff/agents/files"
	"github.com/kiff/agents/store/memory"
)

var (
	ctx = context.Background()
	t0  = time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)
)

type world struct {
	svc   *files.Service
	blobs *memory.Blobs
	now   *time.Time
	logs  *bytes.Buffer
}

func newWorld() *world {
	now := t0
	w := &world{blobs: memory.NewBlobs(), now: &now, logs: &bytes.Buffer{}}
	w.svc = &files.Service{Store: memory.NewFiles(), Blobs: w.blobs, Now: func() time.Time { return *w.now },
		Log: slog.New(slog.NewTextHandler(w.logs, nil))}
	return w
}

func (w *world) deliver(t *testing.T, tenant, agent, run, name, mt, content string) files.File {
	t.Helper()
	f, err := w.svc.Deliver(ctx, tenant, agent, run, "chat-"+run, files.Delivery{Name: name, MediaType: mt, Content: content})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// A name keeps its base only, loses control, format and direction
// characters, ends in its type's extension, and is at most 120 characters.
func TestANameIsCleaned(t *testing.T) {
	for _, c := range []struct{ in, mt, want string }{
		{"report.csv", "text/csv", "report.csv"},
		{"../../etc/passwd", "text/plain", "passwd.txt"},
		{`C:\Users\ana\notes.md`, "text/markdown", "notes.md"},
		{"..", "text/plain", "file.txt"},
		{"", "application/json", "file.json"},
		{"invoice\u202Efdp.exe", "text/csv", "invoicefdp.exe.csv"},
		{"invoice.txt.exe", "text/plain", "invoice.txt.exe.txt"},
		{"zero\u200Bwidth\u2066 isolate\u2069.CSV", "text/csv", "zerowidth isolate.csv"},
		{"tab\there\nline", "text/plain", "tabhereline.txt"},
		{"  . spaced . ", "text/plain", "spaced.txt"},
	} {
		if got := files.Name(c.in, c.mt); got != c.want {
			t.Errorf("Name(%q, %s) = %q, want %q", c.in, c.mt, got, c.want)
		}
	}
	long := files.Name(strings.Repeat("é", 300), "text/csv")
	if n := utf8.RuneCountInString(long); n != files.MaxName || !strings.HasSuffix(long, ".csv") {
		t.Errorf("a long name is %d characters (%q…), want %d ending .csv", n, long[:10], files.MaxName)
	}
}

// Only text types are delivered, as UTF-8 with no NUL; JSON must parse.
func TestOnlyCheckedTextIsKept(t *testing.T) {
	for _, c := range []struct{ mt, content, why string }{
		{"text/html", "<p>hi</p>", "can't be delivered"},
		{"application/pdf", "%PDF-1.7", "can't be delivered"},
		{"text/plain", "", "empty"},
		{"text/plain", "a\x00b", "UTF-8 text"},
		{"text/plain", "\xff\xfe", "UTF-8 text"},
		{"application/json", "{not json", "valid JSON"},
		{"text/csv", "a,\"b\nc", "valid CSV"},
		{"text/csv", "a,=HYPERLINK(\"x\")\n", "valid CSV"},
	} {
		_, err := files.Content(c.mt, []byte(c.content))
		if !errors.Is(err, files.ErrInvalid) || !strings.Contains(err.Error(), c.why) {
			t.Errorf("Content(%s, %q) = %v, want ErrInvalid saying %q", c.mt, c.content, err, c.why)
		}
	}
	big := bytes.Repeat([]byte("a"), files.MaxSize+1)
	if _, err := files.Content("text/plain", big); !errors.Is(err, files.ErrInvalid) || !strings.Contains(err.Error(), "10 MB") {
		t.Errorf("a file over 10 MB: %v", err)
	}
	if _, err := files.Content("text/plain", bytes.Repeat([]byte("a"), files.MaxSize)); err != nil {
		t.Errorf("a file of exactly 10 MB: %v", err)
	}
}

// A CSV cell that would run as a formula comes back as text; numbers stay
// numbers.
func TestACSVFormulaComesBackAsText(t *testing.T) {
	in := "name,amount,link\n" +
		"Ana,-12.50,\"=HYPERLINK(\"\"https://evil.example\"\",\"\"Click\"\")\"\n" +
		"Bo,+33,@SUM(A1:A9)\n" +
		"Cy,\"1,200.00\",+cmd|' /C calc'!A0\n" +
		"Di,-2+3,\t=1+1\n"
	out, err := files.Content("text/csv", []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	want := "name,amount,link\n" +
		"Ana,-12.50,\"'=HYPERLINK(\"\"https://evil.example\"\",\"\"Click\"\")\"\n" +
		"Bo,+33,'@SUM(A1:A9)\n" +
		"Cy,\"1,200.00\",'+cmd|' /C calc'!A0\n" +
		"Di,'-2+3,'\t=1+1\n"
	if string(out) != want {
		t.Errorf("defused CSV:\n%s\nwant:\n%s", out, want)
	}
}

// A preview is plain text: no control or direction characters, cut on a
// whole character at 4 KB.
func TestAPreviewIsPlainText(t *testing.T) {
	got := files.Preview([]byte("Total\u202E 99\r\n\x1b[31mred\x07\tok\u200F"))
	if got != "Total 99\n[31mred\tok" {
		t.Errorf("Preview = %q", got)
	}
	long := strings.Repeat("a", files.MaxPreview-1) + "é and more"
	p := files.Preview([]byte(long))
	if !utf8.ValidString(p) || len(p) != files.MaxPreview-1 {
		t.Errorf("a preview cut mid-character is %d bytes, valid %v", len(p), utf8.ValidString(p))
	}
}

// A delivered file is stored under its own prefix, as its checked type, and
// a download is served as that type under its cleaned name.
func TestADeliveredFileIsKeptAsItsCheckedType(t *testing.T) {
	w := newWorld()
	f := w.deliver(t, "t1", "refunds", "r1", "week.csv", "text/csv", "a,b\n1,2\n")
	if f.Session != "chat-r1" || f.Expires != t0.Add(files.Kept) || f.Name != "week.csv" || f.Size != 8 {
		t.Errorf("delivered: %+v", f)
	}
	if !strings.HasPrefix(f.Key(), "delivered/t1/refunds/fil_") {
		t.Errorf("key %q isn't under delivered/<tenant>/<taf>/", f.Key())
	}
	if got := w.blobs.Type(f.Key()); got != "text/csv; charset=utf-8" {
		t.Errorf("stored as %q", got)
	}
	_, url, err := w.svc.Download(ctx, "t1", f.ID, "member:ana")
	if err != nil || !strings.Contains(url, "name=week.csv") || !strings.Contains(url, "type=text/csv; charset=utf-8") {
		t.Errorf("download %q, %v", url, err)
	}
}

// Every read, download and delete is in the caller's tenant: another
// account's id is not found, and its taf or chat lists nothing.
func TestAnotherAccountsFileIsNeverReached(t *testing.T) {
	w := newWorld()
	f := w.deliver(t, "t1", "refunds", "r1", "week.csv", "text/csv", "a\n")
	if _, err := w.svc.Get(ctx, "t2", f.ID); !errors.Is(err, files.ErrNotFound) {
		t.Errorf("Get from t2: %v", err)
	}
	if _, _, err := w.svc.Read(ctx, "t2", f.ID, "member:eve"); !errors.Is(err, files.ErrNotFound) {
		t.Errorf("Read from t2: %v", err)
	}
	if _, _, err := w.svc.Download(ctx, "t2", f.ID, "member:eve"); !errors.Is(err, files.ErrNotFound) {
		t.Errorf("Download from t2: %v", err)
	}
	if err := w.svc.Delete(ctx, "t2", f.ID, "member:eve"); !errors.Is(err, files.ErrNotFound) {
		t.Errorf("Delete from t2: %v", err)
	}
	for _, q := range []files.Filter{{}, {Agent: "refunds"}, {Run: "r1"}, {Session: "chat-r1"}} {
		if got, err := w.svc.List(ctx, "t2", q); err != nil || len(got) != 0 {
			t.Errorf("t2 lists %+v: %v, %v", q, got, err)
		}
	}
	if _, err := w.svc.Get(ctx, "t1", f.ID); err != nil {
		t.Errorf("t1's own file: %v", err)
	}
}

// A list is newest first, narrowed by taf, run or chat, and hides a file past
// its 30 days, which the sweep then removes, bytes and record.
func TestFilesListNewestFirstAndGoAfter30Days(t *testing.T) {
	w := newWorld()
	a := w.deliver(t, "t1", "refunds", "r1", "a.txt", "text/plain", "a")
	*w.now = t0.Add(time.Hour)
	b := w.deliver(t, "t1", "refunds", "r2", "b.txt", "text/plain", "b")
	*w.now = t0.Add(2 * time.Hour)
	c := w.deliver(t, "t1", "payouts", "r3", "c.txt", "text/plain", "c")
	ids := func(fs []files.File) string {
		var out []string
		for _, f := range fs {
			out = append(out, f.ID)
		}
		return strings.Join(out, " ")
	}
	all, _ := w.svc.List(ctx, "t1", files.Filter{})
	if got, want := ids(all), ids([]files.File{c, b, a}); got != want {
		t.Errorf("list %s, want %s", got, want)
	}
	byTaf, _ := w.svc.List(ctx, "t1", files.Filter{Agent: "refunds"})
	byRun, _ := w.svc.List(ctx, "t1", files.Filter{Run: "r2"})
	byChat, _ := w.svc.List(ctx, "t1", files.Filter{Session: "chat-r3"})
	if ids(byTaf) != ids([]files.File{b, a}) || ids(byRun) != b.ID || ids(byChat) != c.ID {
		t.Errorf("by taf %s, by run %s, by chat %s", ids(byTaf), ids(byRun), ids(byChat))
	}
	*w.now = a.Expires
	if _, err := w.svc.Get(ctx, "t1", a.ID); !errors.Is(err, files.ErrNotFound) {
		t.Errorf("a file at its expiry: %v", err)
	}
	n, err := w.svc.Sweep(ctx, 10)
	if err != nil || n != 1 {
		t.Fatalf("sweep: %d, %v", n, err)
	}
	if keys := w.blobs.Keys(); len(keys) != 2 || slicesContains(keys, a.Key()) {
		t.Errorf("bytes left: %v", keys)
	}
}

func slicesContains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// A run delivers at most five files; the sixth is refused, saying why.
func TestARunDeliversAtMostFive(t *testing.T) {
	w := newWorld()
	for i := range files.MaxPerRun {
		w.deliver(t, "t1", "refunds", "r1", fmt.Sprintf("f%d.txt", i), "text/plain", "x")
	}
	_, err := w.svc.Deliver(ctx, "t1", "refunds", "r1", "", files.Delivery{Name: "f6.txt", MediaType: "text/plain", Content: "x"})
	if !errors.Is(err, files.ErrInvalid) || !strings.Contains(err.Error(), "at most 5") {
		t.Errorf("a sixth file: %v", err)
	}
	if _, err := w.svc.Deliver(ctx, "t1", "refunds", "r2", "", files.Delivery{Name: "f.txt", MediaType: "text/plain", Content: "x"}); err != nil {
		t.Errorf("another run's first file: %v", err)
	}
}

// Past the account's total, by count or by size, its oldest files go first,
// and the new one is kept: a run is never refused for what others left.
func TestTheOldestGoWhenTheAccountIsFull(t *testing.T) {
	w := newWorld()
	w.svc.MaxFiles, w.svc.MaxBytes = 3, 12
	var made []files.File
	for i := range 3 {
		*w.now = t0.Add(time.Duration(i) * time.Minute)
		made = append(made, w.deliver(t, "t1", "refunds", fmt.Sprintf("r%d", i), "f.txt", "text/plain", "abc"))
	}
	w.deliver(t, "t2", "refunds", "other", "f.txt", "text/plain", "abc")
	*w.now = t0.Add(time.Hour)
	d := w.deliver(t, "t1", "refunds", "r9", "f.txt", "text/plain", "abc")
	all, _ := w.svc.List(ctx, "t1", files.Filter{})
	if len(all) != 3 || all[0].ID != d.ID || all[2].ID != made[1].ID {
		t.Errorf("after one over the count: %+v", all)
	}
	if _, err := w.svc.Get(ctx, "t1", made[0].ID); !errors.Is(err, files.ErrNotFound) {
		t.Errorf("the oldest is still there: %v", err)
	}
	// By size: 3+3+3 = 9 bytes kept; 8 more of 12 means two must go.
	*w.now = t0.Add(2 * time.Hour)
	e := w.deliver(t, "t1", "refunds", "r10", "g.txt", "text/plain", "12345678")
	all, _ = w.svc.List(ctx, "t1", files.Filter{})
	if len(all) != 2 || all[0].ID != e.ID || all[1].ID != d.ID {
		t.Errorf("after one over the size: %+v", all)
	}
	if other, _ := w.svc.List(ctx, "t2", files.Filter{}); len(other) != 1 {
		t.Errorf("another account's files went too: %+v", other)
	}
	if !strings.Contains(w.logs.String(), "file removed for room") {
		t.Errorf("a removal for room isn't logged:\n%s", w.logs)
	}
}

// Anyone on the account may delete a file: its bytes go at once, and the log
// names who did it by member id, never the file's name or content.
func TestADeleteLogsWhoNotWhat(t *testing.T) {
	w := newWorld()
	f := w.deliver(t, "t1", "refunds", "r1", "salaries-secret.csv", "text/csv", "Ana,4000\n")
	if _, _, err := w.svc.Read(ctx, "t1", f.ID, "member:bo"); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.Delete(ctx, "t1", f.ID, "member:ana"); err != nil {
		t.Fatal(err)
	}
	if len(w.blobs.Keys()) != 0 {
		t.Errorf("bytes left after delete: %v", w.blobs.Keys())
	}
	if _, err := w.svc.Get(ctx, "t1", f.ID); !errors.Is(err, files.ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
	logs := w.logs.String()
	if !strings.Contains(logs, "msg=\"file deleted\"") || !strings.Contains(logs, "by=member:ana") || !strings.Contains(logs, "file="+f.ID) {
		t.Errorf("the delete's log line doesn't name the member and the file:\n%s", logs)
	}
	if !strings.Contains(logs, "msg=\"file read\"") || !strings.Contains(logs, "by=member:bo") {
		t.Errorf("the read isn't logged by member:\n%s", logs)
	}
	if strings.Contains(logs, "salaries") || strings.Contains(logs, "4000") {
		t.Errorf("a log line carries the file's name or content:\n%s", logs)
	}
}

// Removing a taf removes its files; erasing an account removes all of its.
func TestRemovalTakesTheFiles(t *testing.T) {
	w := newWorld()
	w.deliver(t, "t1", "refunds", "r1", "a.txt", "text/plain", "a")
	p := w.deliver(t, "t1", "payouts", "r2", "b.txt", "text/plain", "b")
	o := w.deliver(t, "t2", "refunds", "r3", "c.txt", "text/plain", "c")
	if err := w.svc.RemoveAgent(ctx, "t1", "refunds"); err != nil {
		t.Fatal(err)
	}
	if all, _ := w.svc.List(ctx, "t1", files.Filter{}); len(all) != 1 || all[0].ID != p.ID {
		t.Errorf("after removing refunds: %+v", all)
	}
	if err := w.svc.EraseTenant(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if all, _ := w.svc.List(ctx, "t1", files.Filter{}); len(all) != 0 {
		t.Errorf("after erasing t1: %+v", all)
	}
	if keys := w.blobs.Keys(); len(keys) != 1 || keys[0] != o.Key() {
		t.Errorf("bytes left: %v, want only t2's", keys)
	}
}
