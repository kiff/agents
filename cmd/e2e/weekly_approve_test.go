package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeTafsCloud struct {
	approval weeklyDecision
	report   weeklyDecision
	asked    map[string]any
	reported map[string]any
}

func (f *fakeTafsCloud) Approval(_ context.Context, _ string, body map[string]any) (weeklyDecision, error) {
	f.asked = body
	return f.approval, nil
}

func (f *fakeTafsCloud) Report(_ context.Context, _, _ string, body map[string]any) (weeklyDecision, error) {
	f.reported = body
	return f.report, nil
}

type fakeWeeklyPhone struct {
	digest    string
	tools     []string
	version   string
	confirmed string
	approved  json.RawMessage
	status    string
}

func (f *fakeWeeklyPhone) Grant(context.Context, string) (string, []string, error) {
	return f.digest, f.tools, nil
}
func (f *fakeWeeklyPhone) DraftVersion(context.Context, string) (string, error) { return f.version, nil }
func (f *fakeWeeklyPhone) ConfirmDraft(_ context.Context, d, v string) error {
	f.confirmed = d + "@" + v
	return nil
}
func (f *fakeWeeklyPhone) ApproveConnect(_ context.Context, _ string, body json.RawMessage) (string, error) {
	f.approved = body
	return f.status, nil
}

var jobTools = []string{"pipedrive_list_persons", "pipedrive_add_activity"}

func signAsk(step, message string) *weeklyAsk {
	a := askConnect("g1", `{"digest":"d1"}`)
	if step == "cards" {
		a = askCards("dr1", "v2")
	}
	a.ID, a.Message = "ask1", message
	return &a
}

func TestAConnectionIsApprovedThroughTheComponentRouteAndReported(t *testing.T) {
	cloud := &fakeTafsCloud{
		approval: weeklyDecision{Outcome: "sign", Ask: signAsk("connect", "tafs.connect.v1|t1|g1|d1")},
		report:   weeklyDecision{Outcome: "answered", Answer: "approved"},
	}
	phone := &fakeWeeklyPhone{digest: "d1", tools: jobTools, status: "connected"}
	got, err := approveCard(context.Background(), cloud, phone, weeklyShown{ID: "c1", Session: "s1", Grant: "g1"}, "t1", jobTools)
	if err != nil || got != "approved" {
		t.Fatalf("got %q, %v", got, err)
	}
	if cloud.asked["shown_digest"] != "d1" || cloud.asked["grant"] != "g1" || cloud.asked["can"] != nil {
		t.Fatalf("the ask sent %v: the digest the phone read, and no `can` (two steps only)", cloud.asked)
	}
	if string(phone.approved) != `{"digest":"d1"}` || cloud.reported["result"] != "connected" || cloud.reported["session"] != "s1" {
		t.Fatalf("approved %s, reported %v", phone.approved, cloud.reported)
	}
}

func TestADraftIsConfirmedAtTheVersionShown(t *testing.T) {
	cloud := &fakeTafsCloud{
		approval: weeklyDecision{Outcome: "sign", Ask: signAsk("cards", "tafs.cards.v1|t1|dr1|v2")},
		report:   weeklyDecision{Outcome: "answered", Answer: "approved"},
	}
	phone := &fakeWeeklyPhone{version: "v2"}
	if _, err := approveCard(context.Background(), cloud, phone, weeklyShown{ID: "c1", Draft: "dr1"}, "t1", jobTools); err != nil {
		t.Fatal(err)
	}
	if phone.confirmed != "dr1@v2" || cloud.reported["result"] != "confirmed" {
		t.Fatalf("confirmed %q, reported %v", phone.confirmed, cloud.reported)
	}
}

func TestTheProofNeverSignsAnAskItCantRebuild(t *testing.T) {
	for name, ask := range map[string]*weeklyAsk{
		"another message":    signAsk("connect", "tafs.connect.v1|t1|g1|other"),
		"another tenant":     signAsk("connect", "tafs.connect.v1|t2|g1|d1"),
		"a step never asked": {ID: "a", Step: "key_and_cards", Message: ""},
	} {
		cloud := &fakeTafsCloud{approval: weeklyDecision{Outcome: "sign", Ask: ask}}
		phone := &fakeWeeklyPhone{digest: "d1", tools: jobTools, status: "connected"}
		_, err := approveCard(context.Background(), cloud, phone, weeklyShown{ID: "c1", Grant: "g1"}, "t1", jobTools)
		if err == nil || phone.approved != nil || cloud.reported != nil {
			t.Errorf("%s: err %v, approved %s, reported %v", name, err, phone.approved, cloud.reported)
		}
	}
}

func TestAConnectionAskingForMoreThanTheJobIsRefusedBeforeAnyAsk(t *testing.T) {
	cloud := &fakeTafsCloud{}
	phone := &fakeWeeklyPhone{digest: "d1", tools: append(jobTools, "pipedrive_add_person")}
	_, err := approveCard(context.Background(), cloud, phone, weeklyShown{ID: "c1", Grant: "g1"}, "t1", jobTools)
	if !errors.Is(err, errMoreThanTheJob) || cloud.asked != nil {
		t.Fatalf("err %v, asked %v", err, cloud.asked)
	}
}

func TestEveryOutcomeButApprovedFailsTheRun(t *testing.T) {
	for name, c := range map[string]*fakeTafsCloud{
		"read_again":       {approval: weeklyDecision{Outcome: "read_again", Note: "out of date"}},
		"answered failed":  {approval: weeklyDecision{Outcome: "answered", Answer: "failed: the grant ended"}},
		"report failed":    {approval: weeklyDecision{Outcome: "sign", Ask: signAsk("connect", "tafs.connect.v1|t1|g1|d1")}, report: weeklyDecision{Outcome: "answered", Answer: "failed: not connected"}},
		"unknown":          {approval: weeklyDecision{Outcome: "maybe"}},
		"sign with no ask": {approval: weeklyDecision{Outcome: "sign"}},
	} {
		phone := &fakeWeeklyPhone{digest: "d1", tools: jobTools, status: "connected"}
		if _, err := approveCard(context.Background(), c, phone, weeklyShown{ID: "c1", Grant: "g1"}, "t1", jobTools); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// A connection that didn't connect is reported as it is, and tafs cloud decides.
	cloud := &fakeTafsCloud{approval: weeklyDecision{Outcome: "sign", Ask: signAsk("connect", "tafs.connect.v1|t1|g1|d1")}, report: weeklyDecision{Outcome: "answered", Answer: "failed: pending"}}
	phone := &fakeWeeklyPhone{digest: "d1", tools: jobTools, status: "pending"}
	_, err := approveCard(context.Background(), cloud, phone, weeklyShown{ID: "c1", Grant: "g1"}, "t1", jobTools)
	if err == nil || cloud.reported["result"] != "not_connected" || cloud.reported["status"] != "pending" || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("err %v, reported %v", err, cloud.reported)
	}
}
