package server_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/run"
)

// opsFixture is the run fixture with an operator check: a request with
// X-Test-Operator: yes is an operator's.
func opsFixture(t *testing.T) *runFixture {
	t.Helper()
	f := newRunFixture(t)
	f.server.Operator = func(r *http.Request) error {
		if r.Header.Get("X-Test-Operator") != "yes" {
			return errors.New("not an operator")
		}
		return nil
	}
	f.restart()
	return f
}

// ops GETs path, as an operator or not, and decodes the JSON answer.
func (f *runFixture) ops(path string, operator bool, tenant string) (int, map[string]any) {
	f.t.Helper()
	req, _ := http.NewRequest("GET", f.srv.URL+path, nil)
	if operator {
		req.Header.Set("X-Test-Operator", "yes")
	}
	if tenant != "" {
		req.Header.Set("X-Test-Tenant", tenant)
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func listed(body map[string]any) (ids, tenants []string) {
	for _, r := range body["runs"].([]any) {
		m := r.(map[string]any)
		ids = append(ids, m["id"].(string))
		tenants = append(tenants, m["tenant"].(string))
	}
	return ids, tenants
}

// Operators read runs across tenants, newest first, and the paused ones
// apart; a tenant's caller can't, however it is verified.
func TestOperatorsReadRunsAcrossTenants(t *testing.T) {
	f := opsFixture(t)
	a := run.New("t1", "payouts", "s1", run.ByOwner, "x", now)
	b := run.New("t2", "refunds", "s2", run.BySchedule, "y", now.Add(time.Second))
	for _, r := range []run.Run{a, b} {
		if err := f.runs.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	held := f.heldOn("exc_1") // t1's payouts, created at now

	code, body := f.ops("/ops/runs", true, "")
	if code != 200 {
		t.Fatalf("list: %d %v", code, body)
	}
	ids, tenants := listed(body)
	if len(ids) != 3 || ids[0] != b.ID || tenants[0] != "t2" {
		t.Fatalf("listed %v in %v: want all three, newest first, each with its tenant", ids, tenants)
	}
	code, body = f.ops("/ops/runs/paused", true, "")
	if ids, _ := listed(body); code != 200 || len(ids) != 1 || ids[0] != held.ID {
		t.Fatalf("paused: %d %v", code, body)
	}
	code, body = f.ops("/ops/runs?limit=1", true, "")
	if ids, _ := listed(body); code != 200 || len(ids) != 1 {
		t.Fatalf("limit 1: %d %v", code, body)
	}
	for _, q := range []string{"0", "101", "x"} {
		if code, _ := f.ops("/ops/runs?limit="+q, true, ""); code != 400 {
			t.Errorf("limit %s: %d, want 400", q, code)
		}
	}

	for name, c := range map[string]struct {
		operator bool
		tenant   string
	}{"no one": {false, ""}, "a tenant's caller": {false, "t1"}} {
		for _, path := range []string{"/ops/runs", "/ops/runs/paused", "/ops/tenants/t1/runs/" + held.ID} {
			if code, _ := f.ops(path, c.operator, c.tenant); code != 401 {
				t.Errorf("%s, %s: %d, want 401", name, path, code)
			}
		}
	}
}

// An operator reads one run of a tenant: its state, what it cost and which
// tools are held, never what anyone wrote or what it said, and never what
// its session recorded.
func TestOperatorsReadARun(t *testing.T) {
	f := opsFixture(t)
	r := f.heldOn("exc_1")
	f.record(r, 1, 2)
	got, _ := f.runs.Get(ctx, "t1", r.ID)
	if _, err := run.Change(ctx, f.runs, got, now, func(n *run.Run) error {
		n.Output, n.Error = "Refunded Ana's order 4471", "card declined for ana@example.com"
		n.Steering = []run.Steering{{Text: "only the €20 one"}}
		n.Requester = run.Requester{Kind: run.ByMember, ID: "ana"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	code, body := f.ops("/ops/tenants/t1/runs/"+r.ID, true, "")
	if code != 200 || body["tenant"] != "t1" || body["status"] != string(run.Paused) || fmt.Sprint(body["held"]) != "[stripe_refund]" {
		t.Fatalf("run: %d %v", code, body)
	}
	for _, k := range []string{"paused", "claim", "lease", "version", "input", "output", "error", "stop", "refusal", "steering", "requester", "requirements", "session"} {
		if _, ok := body[k]; ok {
			t.Errorf("shows %q", k)
		}
	}
	if raw, _ := json.Marshal(body); strings.Contains(string(raw), "ana") || strings.Contains(string(raw), "€20") {
		t.Errorf("an operator reads content: %s", raw)
	}
	if code, _ := f.ops("/ops/tenants/t1/runs/"+r.ID+"/events", true, ""); code != 404 {
		t.Fatalf("a run's events: %d, want 404: no operator route reads them", code)
	}
	if code, _ := f.ops("/ops/tenants/t2/runs/"+r.ID, true, ""); code != 404 {
		t.Fatalf("the run under another tenant: %d, want 404", code)
	}
}

// With no operator check, there are no operators' routes.
func TestNoOperatorNoOps(t *testing.T) {
	f := newRunFixture(t)
	if code, _ := f.ops("/ops/runs", true, "t1"); code != 404 {
		t.Fatalf("%d, want 404", code)
	}
}
