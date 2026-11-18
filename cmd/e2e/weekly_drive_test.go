package main

import (
	"context"
	"encoding/json"
	"errors"
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

var pipedriveFamily = weeklyFamily{Name: "key", Ask: "Connect Pipedrive so a taf can list my people.", Fact: "Use the account testco117.", Tools: jobTools, CapUSD: 3, MaxTurn: 8}

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
	if err := runWeeklyMode(context.Background(), "key", &redacting{w: &strings.Builder{}}, env); err == nil || !strings.Contains(err.Error(), "is required") {
		t.Fatalf("no keys: %v", err)
	}
	if !strings.Contains(weeklyFamilies["key"].Ask, "%s") {
		t.Fatal("the key family's request doesn't name the taf for the sweep")
	}
}
