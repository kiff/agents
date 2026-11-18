package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// unlistedPhone is a phone whose planque doesn't list the grant a card names.
type unlistedPhone struct{ fakeWeeklyPhone }

func (unlistedPhone) Grant(context.Context, string) (string, []weeklyTool, error) {
	return "", nil, errors.New("404: no such grant")
}

func questionsCard(id string, n int) weeklyThreadCard {
	c := card("questions", id)
	for i := 0; i < n; i++ {
		c.Card.Questions = append(c.Card.Questions, question{ID: string(rune('a' + i)), Text: "?", Options: []option{{Value: "v", Label: "V"}}})
	}
	return c
}

func sheetsCard(grant string) weeklyThreadCard {
	a := card("approval", "a1")
	a.Card.Grant = grant
	return a
}

func TestTheSheetsDryRunStopsAtTheCardAndNeverApprovesIt(t *testing.T) {
	chat := &fakeChat{threads: []weeklyThread{
		{Working: true},
		{Cards: []weeklyThreadCard{questionsCard("q1", 4)}},
		{Cards: []weeklyThreadCard{sheetsCard("g1")}},
	}}
	phone := &fakeWeeklyPhone{digest: "d1"}
	r, err := driveToCard(context.Background(), chat, phone, weeklySheets, noWait)
	if err != nil {
		t.Fatal(err)
	}
	if r.Questions != 4 || !r.Card || !r.Listed || len(r.Grants) != 1 || r.Grants[0] != "g1" || chat.started != weeklySheets.Ask {
		t.Fatalf("result %+v, started %q", r, chat.started)
	}
	if phone.approved != nil || phone.confirmed != "" {
		t.Fatalf("the dry run approved the card: %s %q", phone.approved, phone.confirmed)
	}
	// A question asked in words counts too, answered with the owner's one fact.
	chat = &fakeChat{threads: []weeklyThread{{}, {Cards: []weeklyThreadCard{sheetsCard("g1")}}}}
	if r, err = driveToCard(context.Background(), chat, phone, weeklySheets, noWait); err != nil || r.Questions != 1 || chat.sent[0] != weeklySheets.Fact {
		t.Fatalf("result %+v, err %v, sent %q", r, err, chat.sent)
	}
}

func TestTheSheetsDryRunFailsOnWhatTheOwnerSaw(t *testing.T) {
	cases := map[string]struct {
		threads []weeklyThread
		phone   weeklyPhone
		want    error
	}{
		"a card before any question": {[]weeklyThread{{Cards: []weeklyThreadCard{sheetsCard("g1")}}}, &fakeWeeklyPhone{}, errNoQuestions},
		"a failed step": {[]weeklyThread{{Failed: []string{"Coach couldn't connect Google Sheets yet"}, Cards: []weeklyThreadCard{questionsCard("q1", 1)}}},
			&fakeWeeklyPhone{}, errFailedStep},
		"a refused connection": {[]weeklyThread{{Refused: 1}}, &fakeWeeklyPhone{}, errRefused},
		"a grant planque doesn't list": {[]weeklyThread{{Cards: []weeklyThreadCard{questionsCard("q1", 2)}}, {Cards: []weeklyThreadCard{sheetsCard("g1")}}},
			&unlistedPhone{}, errNotListed},
		"a card naming no grant": {[]weeklyThread{{Cards: []weeklyThreadCard{questionsCard("q1", 2)}}, {Cards: []weeklyThreadCard{sheetsCard("")}}},
			&fakeWeeklyPhone{}, errNotListed},
		"a grant the setup agent didn't ask for": {[]weeklyThread{{Cards: []weeklyThreadCard{questionsCard("q1", 2)}}, {Cards: []weeklyThreadCard{sheetsCard("g9")}}},
			&fakeWeeklyPhone{notAsked: map[string]bool{"g9": true}}, errNotAsked},
		"no card in the turns": {[]weeklyThread{{}}, &fakeWeeklyPhone{}, errNoCard},
	}
	for name, c := range cases {
		r, err := driveToCard(context.Background(), &fakeChat{threads: c.threads}, c.phone, weeklySheets, noWait)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err %v, want %v", name, err, c.want)
		}
		if name == "a grant planque doesn't list" && (len(r.Grants) != 1 || r.Listed) {
			t.Errorf("%s: the grant must be recorded for the cleanup, and not listed: %+v", name, r)
		}
		if name == "a grant the setup agent didn't ask for" && len(r.Grants) != 0 {
			t.Errorf("%s: another's grant is never this run's to take away: %+v", name, r)
		}
	}
}

func TestTheThreadSaysWhatFailedAndWhatWasRefused(t *testing.T) {
	th, err := threadOf([]byte(`{"messages":[
		{"text":"hi"},
		{"work":{"steps":[{"tool":"planque","words":"Connect Google Sheets","failed":true,"error":true},{"tool":"planque","not_made":true},{"tool":"read"}]},
		 "refused":{"words":"Coach couldn't connect Google Sheets yet","next":"Try again"}},
		{"work":{"steps":[{"tool":"kiff_call","error":true}]},"refused":null}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(th.Failed, "|") != "Connect Google Sheets|kiff_call" || th.Refused != 1 {
		t.Fatalf("failed %q, refused %d", th.Failed, th.Refused)
	}
}

func TestTheSheetsSummaryHoldsNoID(t *testing.T) {
	r := sheetsResult{Questions: 4, Card: true, Listed: true}
	r.Grants = []string{"grt_secret"}
	r.Turns, r.Spent = 3, 420000
	s := sheetsSummary(r, nil)
	if strings.Contains(s, "grt_secret") || !strings.Contains(s, "PASS") || !strings.Contains(s, "questions Coach asked before its card: 4") {
		t.Fatal(s)
	}
}
