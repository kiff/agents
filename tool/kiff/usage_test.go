package kiff_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/tool/kiff"
)

// usageAPI is KIFF's usage route as the tests see it: it keeps each report
// by its path, and answers code (204 when zero).
type usageAPI struct {
	mu   sync.Mutex
	got  map[string]json.RawMessage
	code int
}

func (u *usageAPI) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			http.Error(w, `{"error":"missing Authorization header"}`, http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPut || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, `{"error":"not this"}`, http.StatusMethodNotAllowed)
			return
		}
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		if u.got == nil {
			u.got = map[string]json.RawMessage{}
		}
		u.got[r.URL.Path] = b
		code := u.code
		u.mu.Unlock()
		if code == 0 {
			code = http.StatusNoContent
		}
		w.WriteHeader(code)
		if code >= 300 {
			_, _ = w.Write([]byte(`{"error":"usage unavailable for ` + testKey + `"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// An ended run's report says what the run recorded: who and what started it,
// when it began and ended, what its turns cost, how it ended (an ERROR as
// failed), its refused calls, and how long it's kept.
func TestAUsageReportSaysWhatTheRunRecorded(t *testing.T) {
	start := time.Date(2026, 11, 7, 8, 0, 0, 0, time.UTC)
	r := run.Run{ID: "4f0c2a9e", Tenant: "t1", Agent: "rae", Session: "s1", Trigger: run.BySchedule, Source: "sch_1",
		Requester: run.Requester{Kind: run.ByMember, ID: "usr_1"}, Status: run.Completed, Stop: "answered",
		Cost: 30_000, Turns: 4, Refused: 1, Declined: 2, Created: start, Ended: start.Add(41 * time.Second),
		Updated: start.Add(time.Hour), Expires: start.Add(7 * 24 * time.Hour)}
	u := kiff.UsageOf(r)
	if u.Agent != "rae" || u.Session != "s1" || u.Trigger != run.BySchedule || u.Source != "sch_1" || u.Requester == nil || u.Requester.ID != "usr_1" ||
		!u.Started.Equal(start) || !u.Ended.Equal(start.Add(41*time.Second)) || u.CostMicroUSD != 30_000 || u.Turns != 4 ||
		u.Status != run.Completed || u.Stop != "answered" || u.Failed || u.Refused != 1 || u.Declined != 2 ||
		u.KeptUntil == nil || !u.KeptUntil.Equal(start.Add(7*24*time.Hour)) {
		t.Fatalf("report %+v", u)
	}
	// Failed is an ERROR; a run kept for good says no end to it, one with no
	// requester names none, and one that ended before runs kept their end
	// says when it was last written.
	r.Status, r.Expires, r.Requester, r.Ended = run.Error, time.Time{}, run.Requester{}, time.Time{}
	if u := kiff.UsageOf(r); !u.Failed || u.KeptUntil != nil || u.Requester != nil || !u.Ended.Equal(r.Updated) {
		t.Fatalf("an ERROR kept for good: %+v", u)
	}
	cancelled := kiff.UsageOf(run.Run{ID: "x", Agent: "rae", Trigger: run.ByOwner, Status: run.Cancelled, Cost: 1})
	if cancelled.Failed {
		t.Fatal("a cancelled run is reported failed")
	}
	b, _ := json.Marshal(cancelled)
	for _, absent := range []string{"requester", "kept_until", "refused", "declined", "session", "source", "stop"} {
		if strings.Contains(string(b), `"`+absent+`"`) {
			t.Errorf("%s is sent empty: %s", absent, b)
		}
	}
}

// A report goes to KIFF's route for its run with the agent's own key; any
// 2xx is taken, and anything else is an error that never carries the key.
func TestAUsageReportGoesToKIFFWithTheAgentsKey(t *testing.T) {
	api := &usageAPI{}
	srv := api.server(t)
	u := kiff.Usage{Agent: "rae", Trigger: run.ByOwner, Status: run.Completed, CostMicroUSD: 30_000, Turns: 2}
	if err := kiff.ReportUsage(context.Background(), srv.Client(), srv.URL+"/", testKey, "4f0c2a9e", u); err != nil {
		t.Fatal(err)
	}
	var got kiff.Usage
	if err := json.Unmarshal(api.got["/v1/me/runs/4f0c2a9e/usage"], &got); err != nil || got.CostMicroUSD != 30_000 || got.Agent != "rae" {
		t.Fatalf("KIFF got %s (%v)", api.got["/v1/me/runs/4f0c2a9e/usage"], err)
	}
	api.code = http.StatusOK
	if err := kiff.ReportUsage(context.Background(), srv.Client(), srv.URL, testKey, "4f0c2a9e", u); err != nil {
		t.Fatalf("a 200: %v", err)
	}
	for _, code := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable} {
		api.code = code
		err := kiff.ReportUsage(context.Background(), srv.Client(), srv.URL, testKey, "4f0c2a9e", u)
		if err == nil || strings.Contains(err.Error(), testKey) || !strings.Contains(err.Error(), "4f0c2a9e") {
			t.Fatalf("a %d: %v", code, err)
		}
	}
	if err := kiff.ReportUsage(context.Background(), srv.Client(), srv.URL, testKey, "", u); err == nil {
		t.Fatal("a report with no run was sent")
	}
}

// The reporter sends with the run's own agent's key; a run whose agent is
// gone can't be reported.
func TestTheUsageReporterUsesTheRunsAgentsKey(t *testing.T) {
	api := &usageAPI{}
	srv := api.server(t)
	report := kiff.UsageReporter(oneAgent{}, keys{}, srv.Client(), srv.URL)
	r := run.Run{ID: "r1", Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Status: run.Completed, Cost: 9_000, Turns: 1}
	if err := report(context.Background(), r); err != nil || api.got["/v1/me/runs/r1/usage"] == nil {
		t.Fatalf("reported: %v, KIFF got %v", err, api.got)
	}
	if err := report(context.Background(), run.Run{ID: "r2", Tenant: "t2", Agent: "rae", Status: run.Completed, Cost: 1}); err == nil {
		t.Fatal("another tenant's agent was reported")
	}
}
