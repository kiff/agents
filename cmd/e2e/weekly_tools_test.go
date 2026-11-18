package main

import (
	"strings"
	"testing"
)

func TestTheTafKeepsExactlyWhatTheSignedCardShowed(t *testing.T) {
	conn := []string{"pipedrive_list_persons_x9k2", "pipedrive_add_activity_x9k2"}
	before := []string{"gmail_send_ab12"}
	if err := toolsKept(before, append(before, conn...), conn, conn); err != nil {
		t.Fatal(err)
	}
	// A new taf (made by the hand-over) had none before.
	if err := toolsKept(nil, conn, conn, conn); err != nil {
		t.Fatal(err)
	}
	err := toolsKept(before, append(before, conn[0]), conn, conn)
	if err == nil || !strings.Contains(err.Error(), "didn't gain the connection's pipedrive_add_activity_x9k2") {
		t.Fatalf("a missing tool: %v", err)
	}
	err = toolsKept(before, append(append(before, conn...), "pipedrive_add_person_x9k2"), conn, conn)
	if err == nil || !strings.Contains(err.Error(), "a tool kept without a Card: pipedrive_add_person_x9k2") {
		t.Fatalf("a tool the card never showed: %v", err)
	}
}

func TestTheTafsToolsAreReadByItsAgent(t *testing.T) {
	raw := []byte(`{"tafs":[{"id":"taf-1","kiff_agent":"agt_a","tools":["x_ab12"]},{"id":"taf-2","kiff_agent":"agt_b"}],"removing":[]}`)
	if got, _ := tafToolsOf(raw, "agt_a"); len(got) != 1 || got[0] != "x_ab12" {
		t.Fatalf("agt_a: %v", got)
	}
	if got, _ := tafToolsOf(raw, "agt_b"); got != nil {
		t.Fatalf("a taf with no field keeps none: %v", got)
	}
	if got, _ := tafToolsOf(raw, "agt_c"); got != nil {
		t.Fatalf("no such taf: %v", got)
	}
}
