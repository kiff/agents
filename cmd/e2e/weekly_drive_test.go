package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeChat is Coach's chat as a script: each read of the thread gives the next
// state, and what the owner sent and answered is kept.
type fakeChat struct {
	threads []weeklyThread
	reads   int
	spent   []int64
	sent    []string
	answers map[string]string
	started string
}

func (f *fakeChat) Start(_ context.Context, text string) (string, error) {
	f.started = text
	return "s1", nil
}
func (f *fakeChat) Send(_ context.Context, _, text string) error {
	f.sent = append(f.sent, text)
	return nil
}
func (f *fakeChat) Thread(context.Context, string) (weeklyThread, error) {
	i := min(f.reads, len(f.threads)-1)
	th := f.threads[i]
	if len(f.spent) > 0 {
		th.Spent = f.spent[min(f.reads, len(f.spent)-1)]
	}
	f.reads++
	return th, nil
}
func (f *fakeChat) Answer(_ context.Context, c string, a json.RawMessage, _ string) error {
	if f.answers == nil {
		f.answers = map[string]string{}
	}
	f.answers[c] = string(a)
	return nil
}

func noWait(context.Context) error { return nil }

var pipedriveFamily = weeklyFamily{Name: "key", Ask: "Connect Pipedrive so a taf can list my people.", Fact: "Use the account testco117.", Job: pipedriveJob, CapUSD: 3, MaxTurn: 8}

func card(typ, id string) weeklyThreadCard {
	return weeklyThreadCard{Session: "s1", Card: component{Type: typ, ID: id}}
}

func TestAWeeklyChatGoesThroughTheComponentRouteToTheHandOver(t *testing.T) {
	approval := card("approval", "a1")
	approval.Card.Grant = "g1"
	result := card("result", "r1")
	result.Card.Taf = &struct {
		Agent        string `json:"agent"`
		Name         string `json:"name"`
		Job          string `json:"job"`
		Draft        string `json:"draft"`
		Improves     string `json:"improves"`
		Note         string `json:"note"`
		Notes        string `json:"notes"`
		Kickoff      string `json:"kickoff"`
		Instructions string `json:"instructions"`
		Disclose     []struct {
			Tool   string   `json:"tool"`
			Fields []string `json:"fields"`
		} `json:"disclose"`
	}{Agent: "agt_new"}
	confirm := card("confirm", "c1")
	confirm.Card.Options = []option{{Value: "yes", Label: "Yes"}, {Value: "change", Label: "Change the limits"}}
	chat := &fakeChat{threads: []weeklyThread{
		{Working: true},
		{Cards: []weeklyThreadCard{confirm}},
		{Cards: []weeklyThreadCard{approval}},
		{Cards: []weeklyThreadCard{result}},
	}}
	cloud := &fakeTafsCloud{approval: weeklyDecision{Outcome: "sign", Ask: signAsk("connect", "tafs.connect.v1|t1|g1|d1")}, report: weeklyDecision{Outcome: "answered", Answer: "approved"}}
	phone := &fakeWeeklyPhone{digest: "d1", tools: jobTools, status: "connected"}
	r, err := driveCoach(context.Background(), chat, cloud, phone, pipedriveFamily, "t1", noWait)
	if err != nil {
		t.Fatal(err)
	}
	if r.Taf != "agt_new" || len(r.Approvals) != 1 || chat.started != pipedriveFamily.Ask {
		t.Fatalf("result %+v, started %q", r, chat.started)
	}
	// The scripted owner never agrees in words: the confirm got its non-agreeing option.
	if chat.answers["c1"] != `"change"` {
		t.Fatalf("the confirm was answered %s", chat.answers["c1"])
	}
}

func TestTheScriptedOwnerNeverApprovesInWords(t *testing.T) {
	c := card("confirm", "c1")
	c.Card.Options = []option{{Value: "yes", Label: "Yes, approve"}, {Value: "ok", Label: "OK"}}
	chat := &fakeChat{threads: []weeklyThread{{Cards: []weeklyThreadCard{c}}}}
	_, err := driveCoach(context.Background(), chat, &fakeTafsCloud{}, &fakeWeeklyPhone{}, pipedriveFamily, "t1", noWait)
	if !errors.Is(err, errInWords) {
		t.Fatalf("err %v; want a card that asks agreement in words to fail the run", err)
	}
	// Coach asking in words gets the owner's fact, never a yes.
	chat = &fakeChat{threads: []weeklyThread{{}}}
	_, _ = driveCoach(context.Background(), chat, &fakeTafsCloud{}, &fakeWeeklyPhone{}, pipedriveFamily, "t1", noWait)
	for _, s := range chat.sent {
		if approvalish.MatchString(s) || s != pipedriveFamily.Fact {
			t.Fatalf("the owner wrote %q", s)
		}
	}
}

func TestNoApprovalCardInTheTurnsFailsTheRun(t *testing.T) {
	chat := &fakeChat{threads: []weeklyThread{{}}}
	_, err := driveCoach(context.Background(), chat, &fakeTafsCloud{}, &fakeWeeklyPhone{}, pipedriveFamily, "t1", noWait)
	if !errors.Is(err, errNoCard) || len(chat.sent) != pipedriveFamily.MaxTurn {
		t.Fatalf("err %v after %d turns", err, len(chat.sent))
	}
}

func TestTheCostCapStopsTheRunBeforeTheNextTurn(t *testing.T) {
	chat := &fakeChat{threads: []weeklyThread{{}}, spent: []int64{1_000_000, 2_500_000, 3_000_000}}
	r, err := driveCoach(context.Background(), chat, &fakeTafsCloud{}, &fakeWeeklyPhone{}, pipedriveFamily, "t1", noWait)
	if !errors.Is(err, errCostCap) || r.Turns != 3 || len(chat.sent) != 2 {
		t.Fatalf("err %v, turns %d, sent %d", err, r.Turns, len(chat.sent))
	}
	if !strings.Contains(err.Error(), "US$3.00 spent") {
		t.Fatalf("the cap's words: %v", err)
	}
}

func TestAHandOverWithNoComponentApprovalFailsTheRun(t *testing.T) {
	chat := &fakeChat{threads: []weeklyThread{{Cards: []weeklyThreadCard{card("result", "r1")}}}}
	if _, err := driveCoach(context.Background(), chat, &fakeTafsCloud{}, &fakeWeeklyPhone{}, pipedriveFamily, "t1", noWait); err == nil {
		t.Fatal("a taf handed over with no approval passed")
	}
}

func confirmCard(agrees string, opts ...option) weeklyThreadCard {
	c := card("confirm", "c1")
	c.Card.Agrees, c.Card.Options = agrees, opts
	return c
}

var (
	agreeOpt  = option{Value: "yes", Label: "Yes, these rules"}
	changeOpt = option{Value: "change", Label: "Change them"}
)

func TestAConfirmAgreeingToAShownNoticeIsTappedAndReported(t *testing.T) {
	notice := card("notice", "n1")
	chat := &fakeChat{threads: []weeklyThread{{Cards: []weeklyThreadCard{notice, confirmCard("n1", agreeOpt, changeOpt)}}, {}}}
	r, _ := driveCoach(context.Background(), chat, &fakeTafsCloud{}, &fakeWeeklyPhone{}, pipedriveFamily, "t1", noWait)
	if chat.answers["c1"] != `"yes"` || len(r.Taps) == 0 || r.Taps[0] != (weeklyTap{Card: "c1", Words: "Yes, these rules", Agrees: "n1"}) {
		t.Fatalf("answered %s, taps %+v", chat.answers["c1"], r.Taps)
	}
}

func TestAConfirmAgreeingToANoticeNotShownIsNeverTapped(t *testing.T) {
	chat := &fakeChat{threads: []weeklyThread{{Cards: []weeklyThreadCard{confirmCard("n9", agreeOpt)}}}}
	_, err := driveCoach(context.Background(), chat, &fakeTafsCloud{}, &fakeWeeklyPhone{}, pipedriveFamily, "t1", noWait)
	if !errors.Is(err, errInWords) || chat.answers["c1"] != "" {
		t.Fatalf("err %v, answered %q", err, chat.answers["c1"])
	}
}

func TestAnAllAgreeingConfirmWithNoNoticeFailsTheRun(t *testing.T) {
	chat := &fakeChat{threads: []weeklyThread{{Cards: []weeklyThreadCard{confirmCard("", agreeOpt, option{Value: "ok", Label: "OK"})}}}}
	if _, err := driveCoach(context.Background(), chat, &fakeTafsCloud{}, &fakeWeeklyPhone{}, pipedriveFamily, "t1", noWait); !errors.Is(err, errInWords) {
		t.Fatalf("err %v", err)
	}
}

func TestAMixedConfirmTakesTheOptionThatDoesntAgree(t *testing.T) {
	chat := &fakeChat{threads: []weeklyThread{{Cards: []weeklyThreadCard{confirmCard("", agreeOpt, changeOpt)}}, {}}}
	r, _ := driveCoach(context.Background(), chat, &fakeTafsCloud{}, &fakeWeeklyPhone{}, pipedriveFamily, "t1", noWait)
	if chat.answers["c1"] != `"change"` || len(r.Taps) == 0 || r.Taps[0].Agrees != "" {
		t.Fatalf("answered %s, taps %+v", chat.answers["c1"], r.Taps)
	}
}

func TestTheWeeklyModeNeedsItsFamilyAndItsKeys(t *testing.T) {
	env := func(string) string { return "" }
	if err := runWeeklyMode(context.Background(), "managed", &redacting{w: &strings.Builder{}}, env); err == nil || !strings.Contains(err.Error(), "no such family") {
		t.Fatalf("a family not set up: %v", err)
	}
	// The account's secrets, which both families share, are never a skip:
	// with the family's own set and the account's not, the run fails.
	own := func(k string) string {
		if k == "PROOF_PIPEDRIVE_KEY" {
			return "set"
		}
		return ""
	}
	if err := runWeeklyMode(context.Background(), "key", &redacting{w: &strings.Builder{}}, own); err == nil || !strings.Contains(err.Error(), "is required") {
		t.Fatalf("no account secrets: %v", err)
	}
	if !strings.Contains(weeklyFamilies["key"].Ask, "%s") {
		t.Fatal("the key family's request doesn't name the taf for the sweep")
	}
}

func TestTheWeeklySummaryNamesTheOutcomeAndTheTaps(t *testing.T) {
	r := weeklyResult{Turns: 4, Spent: 1_200_000, Approvals: []string{"approved"}, Taps: []weeklyTap{{Card: "c1", Words: "Yes, these rules", Agrees: "n1"}, {Card: "c2", Words: "Change them"}}}
	s := weeklySummary(weeklyFamilies["key"], r, nil)
	for _, want := range []string{"key family: PASS", "4 turns, US$1.20 of US$3.00", "1 approvals", `tapped "Yes, these rules", agreeing to a rules notice`, `tapped "Change them"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "n1") || strings.Contains(s, "c1") {
		t.Errorf("the summary names an id:\n%s", s)
	}
	failed := weeklySummary(weeklyFamilies["key"], r, errMoreThanTheJob)
	if !strings.Contains(failed, "FAIL: asked for more than the job") {
		t.Errorf("a failure:\n%s", failed)
	}
}

// weeklyEnv is a job's environment: every secret set but those in unset, and
// the step's summary and outputs in files of the test's own.
func weeklyEnv(t *testing.T, lastSkipped string, unset ...string) (func(string) string, string, string) {
	dir := t.TempDir()
	summary, output := filepath.Join(dir, "summary"), filepath.Join(dir, "output")
	return func(k string) string {
		switch {
		case slices.Contains(unset, k):
			return ""
		case k == "WEEKLY_LAST_SKIPPED":
			return lastSkipped
		case k == "GITHUB_STEP_SUMMARY":
			return summary
		case k == "GITHUB_OUTPUT":
			return output
		}
		return "set"
	}, summary, output
}

func TestAFamilyWithItsOwnSecretsSetIsNeverSkipped(t *testing.T) {
	env, _, output := weeklyEnv(t, "key own-key")
	for _, f := range weeklyFamilyNames() {
		if skipped, err := weeklySkip(weeklyFamilies[f], io.Discard, env); skipped || err != nil {
			t.Fatalf("%s skipped with its secrets set: %v", f, err)
		}
	}
	if b, _ := os.ReadFile(output); len(b) > 0 {
		t.Fatalf("outputs written: %s", b)
	}
}

func TestTheFirstSkipWarnsAndSaysNotProved(t *testing.T) {
	for _, c := range []struct{ family, unset, last string }{
		{"key", "PROOF_PIPEDRIVE_KEY", ""},
		{"own-key", "E2E_DESK_KEY", "key"},
		{"own-key", "E2E_VENDOR", ""},
		// "own-key" skipped last time isn't "key": the names are whole.
		{"key", "PROOF_PIPEDRIVE_KEY", "own-key"},
	} {
		env, summary, output := weeklyEnv(t, c.last, c.unset)
		var out strings.Builder
		skipped, err := weeklySkip(weeklyFamilies[c.family], &out, env)
		if !skipped || err != nil {
			t.Fatalf("%s without %s, last skipped %q: skipped %v, %v", c.family, c.unset, c.last, skipped, err)
		}
		if !strings.HasPrefix(out.String(), "::warning ") || !strings.Contains(out.String(), c.unset+" not set") {
			t.Errorf("the warning: %q", out.String())
		}
		s, _ := os.ReadFile(summary)
		if !strings.Contains(string(s), c.family+" family: skipped, not proved") || strings.Contains(string(s), "PASS") {
			t.Errorf("the summary:\n%s", s)
		}
		if o, _ := os.ReadFile(output); string(o) != "skipped=true\n" {
			t.Errorf("the step's outputs: %q", o)
		}
	}
}

func TestASecondSkipInARowFailsTheJob(t *testing.T) {
	env, summary, output := weeklyEnv(t, "key own-key", "E2E_DESK_KEY")
	skipped, err := weeklySkip(weeklyFamilies["own-key"], io.Discard, env)
	if !skipped || err == nil || !strings.Contains(err.Error(), "skipped twice in a row") {
		t.Fatalf("skipped %v, %v", skipped, err)
	}
	if s, _ := os.ReadFile(summary); !strings.Contains(string(s), "own-key family: FAIL: skipped twice in a row, not proved") {
		t.Errorf("the summary:\n%s", s)
	}
	// Kept skipped, so a third skip fails too.
	if o, _ := os.ReadFile(output); string(o) != "skipped=true\n" {
		t.Errorf("the step's outputs: %q", o)
	}
	// Through the mode itself: it ends there, before any account is reached.
	if err := runWeeklyMode(context.Background(), "own-key", &redacting{w: &strings.Builder{}}, env); err == nil || !strings.Contains(err.Error(), "skipped twice") {
		t.Fatalf("the mode: %v", err)
	}
}

func TestASkipThatCantBeKeptFailsTheJob(t *testing.T) {
	env, _, _ := weeklyEnv(t, "", "PROOF_PIPEDRIVE_KEY")
	bad := func(k string) string {
		if k == "GITHUB_OUTPUT" {
			return filepath.Join(t.TempDir(), "no", "such", "dir", "output")
		}
		return env(k)
	}
	if skipped, err := weeklySkip(weeklyFamilies["key"], io.Discard, bad); !skipped || err == nil {
		t.Fatalf("skipped %v, %v", skipped, err)
	}
}
