package main

import (
	"encoding/json"
	"testing"
)

func askConnect(grant, body string) weeklyAsk {
	a := weeklyAsk{Step: "connect"}
	a.Planque = &struct {
		Grant string          `json:"grant"`
		Body  json.RawMessage `json:"body"`
	}{Grant: grant, Body: json.RawMessage(body)}
	return a
}

func askCards(draft, version string) weeklyAsk {
	a := weeklyAsk{Step: "cards"}
	a.KIFF = &struct {
		Draft   string `json:"draft"`
		Version string `json:"version"`
	}{Draft: draft, Version: version}
	return a
}

func TestThePhoneSignsOnlyTheConnectionTheCardShowed(t *testing.T) {
	c := weeklyCard{Grant: "g1", ShownDigest: "d1"}
	if got := expectedApprovalMessage(askConnect("g1", `{"digest":"d1"}`), c, "t1"); got != "tafs.connect.v1|t1|g1|d1" {
		t.Fatalf("the shown connection: %q", got)
	}
	for name, a := range map[string]weeklyAsk{
		"another grant":       askConnect("g2", `{"digest":"d1"}`),
		"another digest":      askConnect("g1", `{"digest":"d2"}`),
		"a field added":       askConnect("g1", `{"digest":"d1","tools":["x"]}`),
		"profile words added": askConnect("g1", `{"digest":"d1","kiff_profile":{"words":"w"}}`),
		"no body":             askConnect("g1", `null`),
		"a step never asked":  {Step: "key_and_cards"},
	} {
		if got := expectedApprovalMessage(a, c, "t1"); got != "" {
			t.Errorf("%s: signs %q", name, got)
		}
	}
	// Profile words shown are part of the body, and must be exactly those.
	withWords := weeklyCard{Grant: "g1", ShownDigest: "d1", ShownProfile: json.RawMessage(`{"profile_sha256":"p","words_sha256":"w"}`)}
	if expectedApprovalMessage(askConnect("g1", `{"kiff_profile":{"words_sha256":"w","profile_sha256":"p"},"digest":"d1"}`), withWords, "t1") == "" {
		t.Error("the shown words, in another key order, aren't signed")
	}
	if expectedApprovalMessage(askConnect("g1", `{"digest":"d1","kiff_profile":{"profile_sha256":"p","words_sha256":"other"}}`), withWords, "t1") != "" {
		t.Error("other words are signed")
	}
	if expectedApprovalMessage(askConnect("g1", `{"digest":"d1"}`), weeklyCard{Grant: "g1"}, "t1") != "" {
		t.Error("a connection with no digest shown is signed")
	}
}

func TestThePhoneSignsOnlyTheDraftVersionTheCardShowed(t *testing.T) {
	c := weeklyCard{Draft: "dr1", ShownDraft: "v3"}
	if got := expectedApprovalMessage(askCards("dr1", "v3"), c, "t1"); got != "tafs.cards.v1|t1|dr1|v3" {
		t.Fatalf("the shown draft: %q", got)
	}
	for name, a := range map[string]weeklyAsk{"another draft": askCards("dr2", "v3"), "another version": askCards("dr1", "v4"), "no draft": {Step: "cards"}} {
		if got := expectedApprovalMessage(a, c, "t1"); got != "" {
			t.Errorf("%s: signs %q", name, got)
		}
	}
	if expectedApprovalMessage(askCards("dr1", "v3"), weeklyCard{Draft: "dr1"}, "t1") != "" {
		t.Error("a draft with no version shown is signed")
	}
}

func TestAStepIsSignedOnlyAsTheCardDrewIt(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		mark *bool
		step string
		ok   bool
	}{
		{nil, "connect", true}, {nil, "cards", true},
		{&yes, "cards", true}, {&yes, "connect", false},
		{&no, "connect", true}, {&no, "cards", false},
	} {
		if got := stepMatchesMark(c.mark, c.step); got != c.ok {
			t.Errorf("mark %v, step %s: %v, want %v", c.mark, c.step, got, c.ok)
		}
	}
}

func TestAnAskForMoreThanTheJobIsNeverSigned(t *testing.T) {
	fixture := []string{"pipedrive_list_persons", "pipedrive_add_activity"}
	if !sameTools([]string{"pipedrive_add_activity", "pipedrive_list_persons"}, fixture, fixture) {
		t.Fatal("the job's own tools, in another order, aren't signed")
	}
	for name, asked := range map[string][]string{
		"one more":     {"pipedrive_list_persons", "pipedrive_add_activity", "pipedrive_add_person"},
		"another tool": {"pipedrive_list_persons", "pipedrive_add_person"},
		"none":         nil,
	} {
		if sameTools(asked, asked, fixture) {
			t.Errorf("%s: signed", name)
		}
	}
	if sameTools(fixture, []string{"pipedrive_list_persons"}, fixture) {
		t.Error("an ask broader than the card showed is signed")
	}
}
