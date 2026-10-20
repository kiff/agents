package server_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
)

// kiff/tafs#107: a version is stored apart from what's live, any one goes
// live, and a trial of one starts in the background and reads back with
// KIFF's decisions on its writes.
func TestVersionsAndATrial(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	v2 := agentBody("payouts")
	v2.Instructions = "Read the payment before you refund it."
	code, body := f.do("t1", "POST", "/agents/payouts/versions", map[string]any{
		"definition": v2, "note": "reads first", "by": "setup", "draft": "drf_2", "revisions": []string{"rev_2"}})
	f.want(200, code, body, "put version")
	if body["n"] != float64(2) {
		t.Fatalf("put version = %v, want n 2 (the live one kept as 1)", body)
	}
	code, body = f.do("t1", "GET", "/agents/payouts/versions", nil)
	f.want(200, code, body, "list versions")
	vs := body["versions"].([]any)
	if len(vs) != 2 || vs[0].(map[string]any)["live"] != true || vs[1].(map[string]any)["draft"] != "drf_2" {
		t.Fatalf("versions = %v", vs)
	}
	if code, body := f.do("t1", "GET", "/agents/payouts", nil); code != 200 || body["instructions"] == v2.Instructions {
		t.Fatalf("live after a put version = %v, want unchanged", body)
	}

	// The trial: version 2, against its draft, in the background.
	code, body = f.do("t1", "POST", "/agents/payouts/versions/2/trial", map[string]any{"input": "Refund charge ch_1 for 300 cents."})
	f.want(202, code, body, "start trial")
	rn := body["run"].(map[string]any)
	if rn["trigger"] != string(run.ByTrial) || rn["agent_version"] != float64(2) || rn["draft"] != "drf_2" || len(f.background) != 1 {
		t.Fatalf("trial = %v, background %v", rn, f.background)
	}
	if len(f.runner.execs) != 0 {
		t.Fatal("a trial ran in the request")
	}
	id, sess := rn["id"].(string), rn["session"].(string)
	code, body = f.do("t1", "GET", "/agents/payouts/trials/"+id, nil)
	f.want(200, code, body, "read trial")
	if w := body["writes"].([]any); len(w) != 0 {
		t.Fatalf("writes before any = %v", w)
	}
	// The runner records KIFF's decision on a write as the trial goes.
	write := `{"call_id":"c1","tool":"create_refund_k3f9","input":{"charge":"ch_1","amount":300},"decision":"allowed","title":"Create refund"}`
	if err := f.store.Begin(ctx, "t1", sess, id, now); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Append(ctx, "t1", sess, id, []session.Event{{Session: sess, Run: id, Seq: 1, Time: now, Kind: session.TrialWrite, Body: json.RawMessage(write)}}); err != nil {
		t.Fatal(err)
	}
	code, body = f.do("t1", "GET", "/agents/payouts/trials/"+id, nil)
	f.want(200, code, body, "read trial")
	w := body["writes"].([]any)
	if len(w) != 1 {
		t.Fatalf("writes = %v", w)
	}
	got, _ := json.Marshal(w[0])
	if want := `{"arguments":{"amount":300,"charge":"ch_1"},"decision":"allowed","title":"Create refund","tool":"create_refund_k3f9"}`; string(got) != want {
		t.Fatalf("write = %s, want %s", got, want)
	}

	// Live: version 2, then back to 1.
	for _, n := range []int{2, 1} {
		code, body = f.do("t1", "POST", fmt.Sprintf("/agents/payouts/versions/%d/live", n), nil)
		f.want(200, code, body, "switch")
		if _, b := f.do("t1", "GET", "/agents/payouts", nil); b["version"] != float64(n) {
			t.Fatalf("live after switching to %d = v%v", n, b["version"])
		}
	}
}

func TestVersionsRefuseWhatIsntThere(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	for _, c := range []struct{ method, path string }{
		{"POST", "/agents/payouts/versions/9/live"},
		{"POST", "/agents/payouts/versions/x/live"},
		{"GET", "/agents/nobody/versions"},
	} {
		if code, body := f.do("t1", c.method, c.path, nil); code != 404 {
			t.Errorf("%s %s = %d %v, want 404", c.method, c.path, code, body)
		}
	}
	if code, body := f.do("t1", "POST", "/agents/payouts/versions/9/trial", map[string]any{"input": "go"}); code != 404 {
		t.Errorf("trial of a version not there = %d %v", code, body)
	}
	// Another tenant's agent's versions aren't found.
	if code, _ := f.do("t2", "GET", "/agents/payouts/versions", nil); code != 404 {
		t.Errorf("another tenant's versions = %d", code)
	}
	// An ordinary run isn't a trial.
	code, body := f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "Pay", "background": true})
	f.want(202, code, body, "start run")
	if code, _ := f.do("t1", "GET", "/agents/payouts/trials/"+body["id"].(string), nil); code != 404 {
		t.Errorf("an ordinary run read as a trial = %d", code)
	}
}
