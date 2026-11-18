package main

import (
	"testing"
	"time"
)

func TestTheThreadIsReadAsTheAppReadsIt(t *testing.T) {
	th, err := threadOf([]byte(`{"working":"run1","components":[
	  {"session":"s1","run":"r1","component":{"type":"approval","id":"a1","grant":"g1","draft":"d1","tools":["pipedrive_list_persons"],"signs":{"card":true}}},
	  {"session":"s1","run":"r1","component":{"type":"confirm","id":"c1","agrees":"n1","options":[{"value":"yes","label":"Yes"}]},"answer":{"answer":"yes"}},
	  {"session":"s1","component":"not a card"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !th.Working || len(th.Cards) != 2 {
		t.Fatalf("working %v, cards %d", th.Working, len(th.Cards))
	}
	a, c := th.Cards[0], th.Cards[1]
	if a.Card.Grant != "g1" || a.Card.Draft != "d1" || len(a.Tools) != 1 || a.Signs == nil || !*a.Signs || a.Answered {
		t.Fatalf("approval %+v", a)
	}
	if !c.Answered || c.Card.Agrees != "n1" || c.Signs != nil {
		t.Fatalf("confirm %+v", c)
	}
	idle, _ := threadOf([]byte(`{"working":null,"components":[]}`))
	if idle.Working {
		t.Fatal("a chat with no run at work reads as working")
	}
}

func TestOnlyCoachsRunsSinceTheStartCountToTheCap(t *testing.T) {
	since := time.Date(2026, 11, 23, 5, 0, 0, 0, time.UTC)
	raw := []byte(`{"items":[{"at":"2026-11-23T05:10:00Z","credits":40},{"at":"2026-11-23T05:02:00Z","credits":10},{"at":"2026-11-22T20:00:00Z","credits":500}]}`)
	got, err := spentSince(raw, since, 0.01)
	if err != nil || got != 500_000 {
		t.Fatalf("spent %d (%v); want 50 credits at US$0.01 = US$0.50", got, err)
	}
}
