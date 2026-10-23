package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kiff/agents/registry"
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

// kiff/tafs#107: a switch to a version that brings a Card revision begins
// only while no run of the agent is going, holds the definition while the
// owner confirms the revision, and ends when that version goes live, or when
// it's cancelled.
func TestASwitchBeginsWhileNothingRunsAndEndsLive(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	for _, note := range []string{"reads first", "asks first"} {
		v := agentBody("payouts")
		v.Instructions = note
		code, body := f.do("t1", "POST", "/agents/payouts/versions", map[string]any{"definition": v, "note": note, "draft": "drf_" + note[:1]})
		f.want(200, code, body, "put version")
	}

	// A run going: the switch waits for it.
	code, body := f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "Pay", "background": true})
	f.want(202, code, body, "start run")
	going := body["id"].(string)
	code, body = f.do("t1", "POST", "/agents/payouts/versions/2/pending", map[string]any{})
	f.want(409, code, body, "pending while a run goes")
	if _, b := f.do("t1", "GET", "/agents/payouts/versions", nil); b["switching"] != nil {
		t.Fatalf("a refused switch left its mark: %v", b["switching"])
	}
	if err := f.runs.Delete(ctx, "t1", going); err != nil {
		t.Fatal(err)
	}

	code, body = f.do("t1", "POST", "/agents/payouts/versions/2/pending", map[string]any{})
	f.want(200, code, body, "pending")
	sw := body["switching"].(map[string]any)
	if sw["n"] != float64(2) || sw["draft"] != "drf_r" {
		t.Fatalf("switching = %v, want version 2 waiting for its draft", sw)
	}
	code, body = f.do("t1", "POST", "/agents/payouts/versions/2/pending", map[string]any{"draft": "drf_other"})
	f.want(200, code, body, "pending again")
	if body["switching"].(map[string]any)["draft"] != "drf_r" {
		t.Fatalf("pending again = %v, want the first mark", body)
	}
	if _, b := f.do("t1", "GET", "/agents/payouts/versions", nil); b["switching"].(map[string]any)["n"] != float64(2) {
		t.Fatalf("versions while switching = %v", b)
	}
	for _, c := range []struct{ method, path, what string }{
		{"POST", "/agents/payouts/versions/3/pending", "a second switch"},
		{"POST", "/agents/payouts/versions/3/live", "another version live"},
	} {
		if code, body := f.do("t1", c.method, c.path, map[string]any{}); code != 409 {
			t.Errorf("%s while switching = %d %v, want 409", c.what, code, body)
		}
	}
	if code, body := f.do("t1", "PUT", "/agents/payouts", agentBody("payouts")); code != 409 {
		t.Errorf("an upgrade while switching = %d %v, want 409", code, body)
	}
	if code, body := f.do("t1", "POST", "/agents/payouts/versions/1/pending", map[string]any{}); code != 409 {
		t.Errorf("a switch to the live version = %d %v, want 409", code, body)
	}

	code, body = f.do("t1", "POST", "/agents/payouts/versions/2/live", nil)
	f.want(200, code, body, "live")
	if _, b := f.do("t1", "GET", "/agents/payouts/versions", nil); b["switching"] != nil {
		t.Fatalf("still switching after live: %v", b["switching"])
	}

	// Cancelled: the live version stays.
	code, body = f.do("t1", "POST", "/agents/payouts/versions/3/pending", map[string]any{})
	f.want(200, code, body, "pending 3")
	code, body = f.do("t1", "DELETE", "/agents/payouts/switching", nil)
	f.want(200, code, body, "cancel")
	if body["cancelled"] != true {
		t.Fatalf("cancel = %v", body)
	}
	code, body = f.do("t1", "DELETE", "/agents/payouts/switching", nil)
	f.want(200, code, body, "cancel again")
	if body["cancelled"] != false {
		t.Fatalf("cancel with none = %v", body)
	}
	if _, b := f.do("t1", "GET", "/agents/payouts", nil); b["version"] != float64(2) {
		t.Fatalf("live after a cancelled switch = v%v, want 2", b["version"])
	}
	if code, _ := f.do("t2", "DELETE", "/agents/payouts/switching", nil); code != 404 {
		t.Errorf("another tenant's cancel = %d, want 404", code)
	}
}

// A switch sees every run of the agent, however old: a paused run behind
// more than a page of newer ones still holds it back (kiff/tafs#107).
func TestASwitchWaitsForAnOldPausedRun(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	v := agentBody("payouts")
	v.Instructions = "Version two."
	code, body := f.do("t1", "POST", "/agents/payouts/versions", map[string]any{"definition": v, "draft": "drf_2"})
	f.want(200, code, body, "put version")

	old := run.New("t1", "payouts", "sess-old", run.ByOwner, "refund order 42", now.Add(-72*time.Hour))
	old.Status = run.Paused
	if err := f.runs.Create(ctx, old); err != nil {
		t.Fatal(err)
	}
	for i := range 120 {
		r := run.New("t1", "payouts", fmt.Sprintf("sess-%d", i), run.BySchedule, "pay", now.Add(-time.Duration(120-i)*time.Minute))
		r.Status = run.Completed
		if err := f.runs.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	code, body = f.do("t1", "POST", "/agents/payouts/versions/2/pending", map[string]any{})
	f.want(409, code, body, "pending with an old paused run")
	// It names the run in the way, so the app can offer to stop it (kiff/tafs#206).
	blocking, _ := body["blocking"].([]any)
	if len(blocking) != 1 {
		t.Fatalf("blocking %v: want the one paused run", body["blocking"])
	}
	if b := blocking[0].(map[string]any); b["id"] != old.ID || b["session"] != "sess-old" || b["status"] != string(run.Paused) || b["trigger"] != string(run.ByOwner) {
		t.Fatalf("blocking %v", b)
	}
	if _, b := f.do("t1", "GET", "/agents/payouts/versions", nil); b["switching"] != nil {
		t.Fatalf("a refused switch left its mark: %v", b["switching"])
	}

	// Answered and ended, it no longer holds the switch back.
	if err := f.runs.Delete(ctx, "t1", old.ID); err != nil {
		t.Fatal(err)
	}
	code, body = f.do("t1", "POST", "/agents/payouts/versions/2/pending", map[string]any{})
	f.want(200, code, body, "pending")
}

// One version reads back with its definition, for tafs cloud to read its
// instructions at their source (kiff/tafs#107).
func TestAVersionReadsWithItsDefinition(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	v2 := agentBody("payouts")
	v2.Instructions = "Version two."
	code, body := f.do("t1", "POST", "/agents/payouts/versions", map[string]any{"definition": v2, "note": "reads first", "by": "setup", "draft": "drf_2"})
	f.want(200, code, body, "put version")
	code, body = f.do("t1", "GET", "/agents/payouts/versions/2", nil)
	f.want(200, code, body, "get version")
	if v := body["version"].(map[string]any); v["n"] != float64(2) || v["note"] != "reads first" || v["live"] != false {
		t.Fatalf("version = %v", v)
	}
	if d := body["definition"].(map[string]any); d["instructions"] != "Version two." {
		t.Fatalf("definition = %v", d)
	}
	for _, c := range []struct{ tenant, path string }{{"t1", "/agents/payouts/versions/9"}, {"t1", "/agents/payouts/versions/x"}, {"t2", "/agents/payouts/versions/2"}} {
		if code, _ := f.do(c.tenant, "GET", c.path, nil); code != 404 {
			t.Errorf("%s %s = %d, want 404", c.tenant, c.path, code)
		}
	}
}

// A version goes live recording the KIFF Cards it runs under, each at its
// revision, so going back to it can restore them (kiff/tafs#107).
func TestLiveRecordsTheCardsAVersionRunsUnder(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	v2 := agentBody("payouts")
	v2.Instructions = "Version two."
	code, body := f.do("t1", "POST", "/agents/payouts/versions", map[string]any{"definition": v2, "note": "two"})
	f.want(200, code, body, "put version")
	code, body = f.do("t1", "POST", "/agents/payouts/versions/2/live", map[string]any{"cards": []any{map[string]any{"id": "card_pay", "revision": 4}}})
	f.want(200, code, body, "live with cards")
	_, body = f.do("t1", "GET", "/agents/payouts/versions/2", nil)
	if cs, _ := body["version"].(map[string]any)["cards"].([]any); len(cs) != 1 || cs[0].(map[string]any)["revision"] != float64(4) {
		t.Fatalf("version 2 = %v", body["version"])
	}
	// No body still switches, and leaves version 1's record as it was.
	code, body = f.do("t1", "POST", "/agents/payouts/versions/1/live", nil)
	f.want(200, code, body, "live, no body")
	for _, bad := range []map[string]any{
		{"cards": []any{map[string]any{"id": "", "revision": 1}}},
		{"cards": []any{map[string]any{"id": "c", "revision": 0}}},
		{"other": true},
	} {
		if code, b := f.do("t1", "POST", "/agents/payouts/versions/2/live", bad); code != 400 {
			t.Errorf("%v = %d %v, want 400", bad, code, b)
		}
	}
}

// failingCards is a registry whose SetCards fails.
type failingCards struct{ registry.Agents }

func (failingCards) SetCards(context.Context, string, string, int, []registry.CardRevision) error {
	return errors.New("the store is down")
}

// A switch whose Cards can't be recorded leaves nothing switched: the last
// version stays live and the switch still waits, so a retry ends it.
func TestAFailedCardsRecordSwitchesNothing(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	v2 := agentBody("payouts")
	v2.Instructions = "Version two."
	code, body := f.do("t1", "POST", "/agents/payouts/versions", map[string]any{"definition": v2, "note": "two"})
	f.want(200, code, body, "put version")
	code, body = f.do("t1", "POST", "/agents/payouts/versions/2/pending", map[string]any{})
	f.want(200, code, body, "pending")

	f.server.Agents = failingCards{f.agents}
	f.restart()
	cards := map[string]any{"cards": []any{map[string]any{"id": "card_pay", "revision": 2}}}
	if code, body := f.do("t1", "POST", "/agents/payouts/versions/2/live", cards); code < 500 {
		t.Fatalf("live with a failing record = %d %v, want 5xx", code, body)
	}
	_, body = f.do("t1", "GET", "/agents/payouts/versions", nil)
	if vs := body["versions"].([]any); vs[0].(map[string]any)["live"] != true || body["switching"] == nil {
		t.Fatalf("after a failed record: versions %v, switching %v; want 1 live, still switching", vs, body["switching"])
	}

	f.server.Agents = f.agents
	f.restart()
	code, body = f.do("t1", "POST", "/agents/payouts/versions/2/live", cards)
	f.want(200, code, body, "retry")
	if _, b := f.do("t1", "GET", "/agents/payouts/versions", nil); b["switching"] != nil {
		t.Fatalf("the retry left the mark: %v", b["switching"])
	}
}

// A switch drafted again waits for the new draft with no gap: the mark stays,
// so nothing of the agent can start in between (kiff/tafs#107).
func TestASwitchsDraftIsReplacedInPlace(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	v2 := agentBody("payouts")
	v2.Instructions = "Version two."
	code, body := f.do("t1", "POST", "/agents/payouts/versions", map[string]any{"definition": v2, "note": "two", "draft": "drf_old"})
	f.want(200, code, body, "put version")
	// Nothing to replace before the switch begins.
	code, body = f.do("t1", "POST", "/agents/payouts/versions/2/pending", map[string]any{"draft": "drf_new", "replace": true})
	f.want(409, code, body, "replace with no switch")
	code, body = f.do("t1", "POST", "/agents/payouts/versions/2/pending", map[string]any{})
	f.want(200, code, body, "pending")
	at := body["switching"].(map[string]any)["at"]

	// Replaced: the same switch, never dropped in between.
	code, body = f.do("t1", "POST", "/agents/payouts/versions/2/pending", map[string]any{"draft": "drf_new", "replace": true})
	f.want(200, code, body, "replace")
	sw := body["switching"].(map[string]any)
	if sw["draft"] != "drf_new" || sw["n"] != float64(2) || sw["at"] != at {
		t.Fatalf("replaced switch = %v, want version 2 waiting for drf_new, begun when it was", sw)
	}
	if _, b := f.do("t1", "GET", "/agents/payouts/versions", nil); b["switching"] == nil || b["switching"].(map[string]any)["draft"] != "drf_new" {
		t.Fatalf("after the replace, switching = %v; want the mark kept, waiting for drf_new", b["switching"])
	}
	if code, b := f.do("t1", "POST", "/agents/payouts/versions/2/pending", map[string]any{"replace": true}); code != 400 {
		t.Fatalf("replace with no draft = %d %v, want 400", code, b)
	}
	// A trial meanwhile asks KIFF about the draft the switch waits for, not
	// the one the version was stored with.
	code, body = f.do("t1", "POST", "/agents/payouts/versions/2/trial", map[string]any{"input": "Refund charge ch_1 for 300 cents."})
	f.want(202, code, body, "trial while switching")
	if d := body["run"].(map[string]any)["draft"]; d != "drf_new" {
		t.Fatalf("trial while switching runs under %v, want drf_new", d)
	}
}
