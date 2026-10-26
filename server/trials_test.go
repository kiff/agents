package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
)

// putVersionTwo stores version 2 of payouts, not live.
func (f *runFixture) putVersionTwo() {
	f.t.Helper()
	v2 := agentBody("payouts")
	v2.Instructions = "Read the payment before you refund it."
	code, body := f.do("t1", "POST", "/agents/payouts/versions", map[string]any{"definition": v2, "note": "reads first", "draft": "drf_2"})
	f.want(200, code, body, "put version")
}

// kiff/tafs#173: a trial is one chat with version n, under its trial Card's
// draft, beside the live version. Ending it deletes the chat, and the version
// keeps one line with KIFF's counts.
func TestATrialIsOneChatWithAVersion(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.putVersionTwo()

	code, body := f.do("t1", "PUT", "/agents/payouts/trial", map[string]any{"n": 2, "draft": "drf_t2"})
	f.want(200, code, body, "open trial")
	tr := body["trial"].(map[string]any)
	if tr["n"] != float64(2) || tr["draft"] != "drf_t2" || tr["session"] == nil || tr["id"] == "" {
		t.Fatalf("trial = %v", tr)
	}
	id := tr["id"]
	// Opened again the same way, it is the same trial; another one waits for this one's end.
	code, body = f.do("t1", "PUT", "/agents/payouts/trial", map[string]any{"n": 2, "draft": "drf_t2"})
	f.want(200, code, body, "open the same trial again")
	if body["trial"].(map[string]any)["id"] != id {
		t.Fatalf("the same trial opened again is another: %v", body)
	}
	code, body = f.do("t1", "PUT", "/agents/payouts/trial", map[string]any{"n": 1, "draft": "drf_t1"})
	f.want(409, code, body, "open another trial")

	// Two messages: one session, version 2, the trial Card's draft, in the background.
	var sess string
	for i, input := range []string{"Refund ch_1.", "And ch_2."} {
		code, body = f.do("t1", "POST", "/agents/payouts/trial/messages", map[string]any{"input": input})
		f.want(202, code, body, "trial message")
		rn := body["run"].(map[string]any)
		if rn["trigger"] != string(run.ByTrial) || rn["agent_version"] != float64(2) || rn["draft"] != "drf_t2" {
			t.Fatalf("trial run = %v", rn)
		}
		if i == 0 {
			sess = rn["session"].(string)
		} else if rn["session"] != sess {
			t.Fatalf("the second message ran in session %v, not the trial's %s", rn["session"], sess)
		}
		if got := body["trial"].(map[string]any)["session"]; got != sess {
			t.Fatalf("the trial's session = %v, want %s", got, sess)
		}
	}
	if len(f.background) != 2 || len(f.runner.execs) != 0 {
		t.Fatalf("background %v, executed in the request %v", f.background, f.runner.execs)
	}
	code, body = f.do("t1", "GET", "/agents/payouts/trial", nil)
	f.want(200, code, body, "read trial")
	if body["trial"].(map[string]any)["session"] != sess {
		t.Fatalf("trial = %v", body)
	}
	// The live version is untouched.
	if _, b := f.do("t1", "GET", "/agents/payouts", nil); b["version"] != float64(1) {
		t.Fatalf("live = v%v, want 1", b["version"])
	}
	if code, _ := f.do("t2", "GET", "/agents/payouts/trial", nil); code != 404 {
		t.Fatalf("another tenant read the trial: %d", code)
	}

	// Go back: the chat is deleted, version 2 keeps a line with KIFF's counts.
	code, body = f.do("t1", "DELETE", "/agents/payouts/trial", map[string]any{"why": "back", "calls": 3, "held": 1, "refused": 0})
	f.want(200, code, body, "end trial")
	if code, _ := f.do("t1", "GET", "/sessions/"+sess+"?as=service", nil); code != 404 {
		t.Fatalf("the trial's chat after its end = %d, want 404", code)
	}
	if code, _ := f.do("t1", "GET", "/agents/payouts/trial", nil); code != 404 {
		t.Fatalf("the trial after its end = %d, want 404", code)
	}
	code, body = f.do("t1", "GET", "/agents/payouts/versions", nil)
	f.want(200, code, body, "versions")
	vs := body["versions"].([]any)
	got, _ := json.Marshal(vs[1].(map[string]any)["trials"])
	if want := `[{"calls":3,"ended":"2026-10-16T08:00:00Z","held":1,"refused":0,"started":"2026-10-16T08:00:00Z","why":"back"}]`; string(got) != want {
		t.Fatalf("version 2's trials = %s, want %s", got, want)
	}
	if vs[0].(map[string]any)["trials"] != nil {
		t.Fatalf("version 1 has trials: %v", vs[0])
	}
	if code, _ := f.do("t1", "DELETE", "/agents/payouts/trial", map[string]any{"why": "back"}); code != 404 {
		t.Fatalf("ending it again = %d, want 404", code)
	}
	// Once ended, another trial can open.
	code, body = f.do("t1", "PUT", "/agents/payouts/trial", map[string]any{"n": 1, "draft": "drf_t1"})
	f.want(200, code, body, "open another trial after the end")
}

func TestATrialRefusesWhatIsntThere(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.putVersionTwo()
	for _, c := range []struct {
		method, path string
		body         any
		want         int
	}{
		{"PUT", "/agents/payouts/trial", map[string]any{"n": 9, "draft": "drf_t9"}, 404},
		{"PUT", "/agents/nobody/trial", map[string]any{"n": 1, "draft": "drf_t1"}, 404},
		{"PUT", "/agents/payouts/trial", map[string]any{"n": 2}, 400},
		{"GET", "/agents/payouts/trial", nil, 404},
		{"POST", "/agents/payouts/trial/messages", map[string]any{"input": "go"}, 404},
		{"DELETE", "/agents/payouts/trial", map[string]any{"why": "back"}, 404},
	} {
		if code, body := f.do("t1", c.method, c.path, c.body); code != c.want {
			t.Errorf("%s %s = %d %v, want %d", c.method, c.path, code, body, c.want)
		}
	}
	code, body := f.do("t1", "PUT", "/agents/payouts/trial", map[string]any{"n": 2, "draft": "drf_t2"})
	f.want(200, code, body, "open trial")
	for _, b := range []map[string]any{{"why": "bored"}, {"why": "back", "calls": -1}} {
		if code, body := f.do("t1", "DELETE", "/agents/payouts/trial", b); code != 400 {
			t.Errorf("end with %v = %d %v, want 400", b, code, body)
		}
	}
	if code, body := f.do("t1", "POST", "/agents/payouts/trial/messages", map[string]any{"input": ""}); code != 400 {
		t.Errorf("an empty message = %d %v", code, body)
	}
}

// A trial unused for a week takes no more messages, and the sweep ends it:
// KIFF ends its trial Card first, with the agent's own key, and its counts
// are kept on the version. One KIFF can't end now stays for the next sweep.
func TestTheSweepEndsIdleTrialsInKIFFFirst(t *testing.T) {
	f := newRunFixture(t)
	var asked []string
	kiffDown := false
	if ended, err := f.server.SweepTrials(ctx); err != nil || len(ended) != 0 {
		t.Fatalf("a sweep without KIFF = %v, %v; want nothing ended", ended, err)
	}
	f.server.EndKIFFTrial = func(_ context.Context, t registry.Trial) (registry.TrialCounts, error) {
		asked = append(asked, t.Tenant+"/"+t.Agent+"/"+t.Draft)
		if kiffDown {
			return registry.TrialCounts{}, errors.New("KIFF is down")
		}
		return registry.TrialCounts{Calls: 2, Refused: 1}, nil
	}
	f.putAgent("t1", "payouts")
	f.putAgent("t1", "bills")
	f.putVersionTwo()
	old := now.Add(-registry.TrialIdle - time.Minute)
	idle := registry.Trial{Tenant: "t1", Agent: "payouts", ID: "tr_old", N: 2, Draft: "drf_t2", Started: old, Used: old}
	fresh := registry.Trial{Tenant: "t1", Agent: "bills", ID: "tr_new", N: 1, Draft: "drf_b1", Started: now, Used: now}
	for _, tr := range []registry.Trial{idle, fresh} {
		if err := f.agents.OpenTrial(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}
	code, body := f.do("t1", "POST", "/agents/payouts/trial/messages", map[string]any{"input": "still there?"})
	f.want(409, code, body, "a message to an idle trial")

	kiffDown = true
	if ended, err := f.server.SweepTrials(ctx); err == nil || len(ended) != 0 {
		t.Fatalf("sweep with KIFF down = %v, %v; want its error and nothing ended", ended, err)
	}
	if _, ok, _ := f.agents.Trial(ctx, "t1", "payouts"); !ok {
		t.Fatal("a trial KIFF couldn't end was ended here")
	}

	kiffDown = false
	ended, err := f.server.SweepTrials(ctx)
	if err != nil || len(ended) != 1 || ended[0].Agent != "payouts" {
		t.Fatalf("sweep = %+v, %v", ended, err)
	}
	if want := []string{"t1/payouts/drf_t2", "t1/payouts/drf_t2"}; len(asked) != 2 || asked[0] != want[0] || asked[1] != want[1] {
		t.Fatalf("KIFF asked to end %v, want only the idle one, twice", asked)
	}
	if _, ok, _ := f.agents.Trial(ctx, "t1", "payouts"); ok {
		t.Fatal("the idle trial is still open")
	}
	if _, ok, _ := f.agents.Trial(ctx, "t1", "bills"); !ok {
		t.Fatal("the sweep ended a trial in use")
	}
	var vs []registry.Version
	vs, err = f.agents.Versions(ctx, "t1", "payouts")
	if err != nil || len(vs[1].Trials) != 1 {
		t.Fatalf("version 2 = %+v, %v", vs, err)
	}
	if s := vs[1].Trials[0]; s.Why != registry.TrialIdled || s.Calls != 2 || s.Refused != 1 || !s.Started.Equal(old) {
		t.Fatalf("summary = %+v", s)
	}
	if _, err := f.server.SweepTrials(ctx); err != nil || len(asked) != 2 {
		t.Fatalf("a second sweep asked KIFF again: %v, %v", err, asked)
	}
}

// kiff/agents#215 review: a trial's chat is chosen when it opens, so two first
// messages share it; and a message to a trial that has ended takes nothing
// and doesn't bring it back.
func TestATrialsChatIsChosenWhenItOpens(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.putVersionTwo()
	code, body := f.do("t1", "PUT", "/agents/payouts/trial", map[string]any{"n": 2, "draft": "drf_t2"})
	f.want(200, code, body, "open trial")
	sess := body["trial"].(map[string]any)["session"]
	if sess == nil || sess == "" {
		t.Fatalf("an open trial has no chat yet: %v", body)
	}
	for _, input := range []string{"Refund ch_1.", "And ch_2."} {
		code, body = f.do("t1", "POST", "/agents/payouts/trial/messages", map[string]any{"input": input})
		f.want(202, code, body, "trial message")
		if got := body["run"].(map[string]any)["session"]; got != sess {
			t.Fatalf("a message ran in %v, not the trial's chat %v", got, sess)
		}
	}

	code, body = f.do("t1", "DELETE", "/agents/payouts/trial", map[string]any{"why": "back"})
	f.want(200, code, body, "end trial")
	code, body = f.do("t1", "POST", "/agents/payouts/trial/messages", map[string]any{"input": "one more"})
	f.want(404, code, body, "a message after the trial ended")
	if _, ok, _ := f.agents.Trial(context.Background(), "t1", "payouts"); ok {
		t.Fatal("a message after the end brought the trial back")
	}
}

// Two opens at once, of different versions, give one trial and a 409, and
// leave one trial chat: the one that lost removes the chat it opened.
func TestTwoOpensAtOnceGiveOneTrial(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.putVersionTwo()
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for _, open := range []map[string]any{{"n": 2, "draft": "drf_t2"}, {"n": 1, "draft": "drf_t1"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _ := f.do("t1", "PUT", "/agents/payouts/trial", open)
			codes <- code
		}()
	}
	wg.Wait()
	close(codes)
	var got []int
	for c := range codes {
		got = append(got, c)
	}
	slices.Sort(got)
	if !slices.Equal(got, []int{200, 409}) {
		t.Fatalf("two opens at once answered %v; want one 200 and one 409", got)
	}
	tr, ok, err := f.agents.Trial(context.Background(), "t1", "payouts")
	if err != nil || !ok {
		t.Fatalf("no trial after two opens: %v", err)
	}
	all, err := f.store.List(context.Background(), "t1", "payouts", 10)
	if err != nil {
		t.Fatal(err)
	}
	var trials []string
	for _, s := range all {
		if s.Kind == session.KindTrial {
			trials = append(trials, s.ID)
		}
	}
	if !slices.Equal(trials, []string{tr.Session}) {
		t.Fatalf("trial chats = %v; want only the open trial's %s", trials, tr.Session)
	}
}
