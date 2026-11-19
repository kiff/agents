package server_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/files"
	"github.com/kiff/agents/store/memory"
)

// withDelivered serves the API with delivered files in memory.
func (f *runFixture) withDelivered() *files.Service {
	svc := &files.Service{Store: memory.NewFiles(), Blobs: memory.NewBlobs(), Now: func() time.Time { return now }}
	f.server.Delivered = svc
	f.restart()
	return svc
}

func deliverOne(t *testing.T, svc *files.Service, tenant, agent, run, session string) files.File {
	t.Helper()
	fl, err := svc.Deliver(context.Background(), tenant, agent, run, session,
		files.Delivery{Name: "week.csv", MediaType: "text/csv", Content: "name,link\nAna,\"=HYPERLINK(\"\"x\"\")\"\n"})
	if err != nil {
		t.Fatal(err)
	}
	return fl
}

// An account lists, reads, downloads and deletes its own files; another
// account's id is not found on every route, and its taf, run or chat lists
// nothing.
func TestAnAccountReachesOnlyItsOwnFiles(t *testing.T) {
	f := newRunFixture(t)
	svc := f.withDelivered()
	mine := deliverOne(t, svc, "t1", "refunds", "r1", "s1")
	theirs := deliverOne(t, svc, "t2", "payouts", "r9", "s9")

	code, body := f.do("t1", "GET", "/files", nil)
	if code != http.StatusOK || len(body["files"].([]any)) != 1 || body["files"].([]any)[0].(map[string]any)["id"] != mine.ID {
		t.Fatalf("t1's list: %d %v", code, body)
	}
	for _, q := range []string{"?taf=payouts", "?run=r9", "?chat=s9"} {
		if code, body := f.do("t1", "GET", "/files"+q, nil); code != http.StatusOK || len(body["files"].([]any)) != 0 {
			t.Errorf("t1 lists t2's %s: %d %v", q, code, body)
		}
	}
	for _, r := range []struct{ method, path string }{
		{"GET", "/files/" + theirs.ID + "?by=member:eve"},
		{"GET", "/files/" + theirs.ID + "/download?by=member:eve"},
		{"DELETE", "/files/" + theirs.ID + "?by=member:eve"},
	} {
		if code, body := f.do("t1", r.method, r.path, nil); code != http.StatusNotFound {
			t.Errorf("%s %s from t1: %d %v, want 404", r.method, r.path, code, body)
		}
	}
	if _, err := svc.Get(context.Background(), "t2", theirs.ID); err != nil {
		t.Errorf("t2's file after t1's tries: %v", err)
	}

	code, body = f.do("t1", "GET", "/files/"+mine.ID+"?by=member:ana", nil)
	if code != http.StatusOK || body["taf"] != "refunds" || body["chat"] != "s1" || body["run"] != "r1" ||
		!strings.Contains(body["preview"].(string), "'=HYPERLINK") {
		t.Errorf("t1 reads its own: %d %v", code, body)
	}
	code, body = f.do("t1", "GET", "/files/"+mine.ID+"/download?by=member:ana", nil)
	if code != http.StatusOK || !strings.Contains(body["url"].(string), "type=text/csv; charset=utf-8") {
		t.Errorf("t1 downloads its own: %d %v", code, body)
	}
	if code, body := f.do("t1", "DELETE", "/files/"+mine.ID+"?by=member:ana", nil); code != http.StatusNoContent {
		t.Errorf("t1 deletes its own: %d %v", code, body)
	}
	if code, _ := f.do("t1", "GET", "/files/"+mine.ID+"?by=member:ana", nil); code != http.StatusNotFound {
		t.Errorf("after delete: %d", code)
	}
}

// Reading, downloading or deleting names the member, so the log says who.
func TestAFileRouteNamesTheMember(t *testing.T) {
	f := newRunFixture(t)
	svc := f.withDelivered()
	fl := deliverOne(t, svc, "t1", "refunds", "r1", "")
	for _, by := range []string{"", "?by=service", "?by=door:d1", "?by=member:"} {
		for _, r := range []struct{ method, path string }{
			{"GET", "/files/" + fl.ID + by}, {"GET", "/files/" + fl.ID + "/download" + by}, {"DELETE", "/files/" + fl.ID + by},
		} {
			if code, _ := f.do("t1", r.method, r.path, nil); code != http.StatusBadRequest {
				t.Errorf("%s %s: %d, want 400", r.method, r.path, code)
			}
		}
	}
}

// A list is a page at a time, newest first: next names where the next page
// starts.
func TestFilesListAPageAtATime(t *testing.T) {
	f := newRunFixture(t)
	svc := f.withDelivered()
	svc.MaxFiles = 1000
	for i := range 102 {
		svc.Now = func() time.Time { return now.Add(time.Duration(i) * time.Second) }
		deliverOne(t, svc, "t1", "refunds", "r"+string(rune('a'+i%26))+string(rune('a'+i/26)), "")
	}
	svc.Now = func() time.Time { return now.Add(time.Hour) }
	code, body := f.do("t1", "GET", "/files", nil)
	if code != http.StatusOK || len(body["files"].([]any)) != 100 || body["next"] == nil {
		t.Fatalf("first page: %d, %d files, next %v", code, len(body["files"].([]any)), body["next"])
	}
	code, body = f.do("t1", "GET", "/files?after="+body["next"].(string), nil)
	if code != http.StatusOK || len(body["files"].([]any)) != 2 || body["next"] != nil {
		t.Errorf("second page: %d, %v", code, body)
	}
	if code, _ := f.do("t1", "GET", "/files?after=fil_nope", nil); code != http.StatusBadRequest {
		t.Errorf("after an unknown id: %d", code)
	}
}

// Without delivered files, the routes say so.
func TestNoDeliveredFilesIs503(t *testing.T) {
	f := newRunFixture(t)
	if code, _ := f.do("t1", "GET", "/files", nil); code != http.StatusServiceUnavailable {
		t.Errorf("GET /files: %d", code)
	}
}

// Removing a taf removes its delivered files; erasing the account removes
// all of them, and another account's stay.
func TestRemovalThroughTheAPITakesTheFiles(t *testing.T) {
	f := newRunFixture(t)
	svc := f.withDelivered()
	f.putAgent("t1", "payouts")
	f.putAgent("t1", "refunds")
	deliverOne(t, svc, "t1", "payouts", "r1", "")
	kept := deliverOne(t, svc, "t1", "refunds", "r2", "")
	other := deliverOne(t, svc, "t2", "payouts", "r3", "")
	if code, body := f.do("t1", "DELETE", "/agents/payouts", nil); code != http.StatusNoContent {
		t.Fatalf("removing payouts: %d %v", code, body)
	}
	if all, _ := svc.List(context.Background(), "t1", files.Filter{}); len(all) != 1 || all[0].ID != kept.ID {
		t.Errorf("after removing payouts: %+v", all)
	}
	if code, body := f.do("t1", "DELETE", "/tenant", nil); code != http.StatusNoContent {
		t.Fatalf("erasing t1: %d %v", code, body)
	}
	if all, _ := svc.List(context.Background(), "t1", files.Filter{}); len(all) != 0 {
		t.Errorf("after erasing t1: %+v", all)
	}
	if _, err := svc.Get(context.Background(), "t2", other.ID); err != nil {
		t.Errorf("t2's file after t1's erasure: %v", err)
	}
}

// Through the API, a read and a delete are logged with the member who asked.
func TestTheAPILogsWhoReadAndDeleted(t *testing.T) {
	f := newRunFixture(t)
	svc := f.withDelivered()
	logs := &bytes.Buffer{}
	svc.Log = slog.New(slog.NewTextHandler(logs, nil))
	fl := deliverOne(t, svc, "t1", "refunds", "r1", "")
	f.do("t1", "GET", "/files/"+fl.ID+"?by=member:bo", nil)
	f.do("t1", "DELETE", "/files/"+fl.ID+"?by=member:ana", nil)
	for _, want := range []string{`msg="file read" tenant=t1 file=` + fl.ID + " by=member:bo", `msg="file deleted" tenant=t1 file=` + fl.ID + " by=member:ana"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("no %q in:\n%s", want, logs)
		}
	}
}
