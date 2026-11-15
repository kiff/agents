package eval

import (
	"strings"
	"testing"
)

// The record a judge reads ends with its calls counted by tool, in the
// order the tools first appear: earlier calls and uncounted ones left out,
// the calls with no word on their effect named only when there are some.
// A record whose calls carry no count has no counts.
func TestRecordedTextCounts(t *testing.T) {
	calls := []RecordedCall{
		{Tool: "get", Outcome: "done", Earlier: true, Count: "done"},
		{Tool: "refund", Outcome: "done", Count: "done"},
		{Tool: "refund", Outcome: "refused by KIFF, not sent", Count: "refused"},
		{Tool: "refund", Outcome: "done", Count: "done"},
		{Tool: "mail", Outcome: "sent, but whether it took effect is unknown", Count: "unknown"},
		{Tool: "mail", Outcome: "held by KIFF for the owner", Count: "held"},
		{Tool: "refund", Outcome: "not made: it was sent beside a read"},
		{Tool: "refund", Outcome: "failed", Count: "failed"},
	}
	got := RecordedText(calls, "Two refunded.")
	want := "8. refund  → failed\n\nCounts:\nrefund: 2 done, 0 held, 1 refused, 1 failed\nmail: 0 done, 1 held, 0 refused, 0 failed, 1 unknown\n\nText:\nTwo refunded."
	if !strings.HasSuffix(got, want) {
		t.Fatalf("the judge reads\n%s\nwant it to end\n%s", got, want)
	}
	for i := range calls {
		calls[i].Count = ""
	}
	if got := RecordedText(calls, "x"); strings.Contains(got, "Counts:") {
		t.Fatalf("a record with no counted call has counts:\n%s", got)
	}
	if !strings.Contains(Recorded, "is as the counts have it: any other number contradicts the record") {
		t.Fatal("the rule doesn't check a number against the counts")
	}
}

// Eval 43695849959's chargebacks: the taf said "$150 is above the $100 limit
// per close", and the record the judge read had no $100. The agent's own Card
// read is kept whole in the record, any other call's outcome cut as before,
// and the rule names a limit among the details a record neither confirms nor
// contradicts.
func TestTheCardsReadIsKeptWholeInTheRecord(t *testing.T) {
	limits := strings.Repeat("Accept a dispute: up to $100 per close, refused above. ", 8)
	lines := RecordLines([]RecordedCall{
		{Tool: "kiff_card", Input: "{}", Outcome: limits},
		{Tool: "stripe__retrieve_dispute", Input: `{"id":"dp_1"}`, Outcome: limits},
	})
	parts := strings.Split(lines, "\n")
	if !strings.Contains(parts[0], strings.TrimSpace(limits)) || strings.HasSuffix(parts[0], "…") {
		t.Fatalf("the Card read was cut: %s", parts[0])
	}
	if !strings.HasSuffix(parts[1], "…") {
		t.Fatalf("another call's outcome wasn't cut: %s", parts[1])
	}
	for _, name := range []string{"kiff_card", "kiff__kiff_card", "gw.kiff_card"} {
		if !cardRead(name) {
			t.Errorf("%s isn't read as the Card", name)
		}
	}
	if cardRead("stripe_card") || cardRead("kiff_cards_list") {
		t.Error("a vendor's card tool read as the agent's Card")
	}
	if !strings.Contains(Recorded, "a total, a limit or what a look-up found, is neither confirmed nor contradicted") {
		t.Error("the rule doesn't name a limit as a detail the record neither confirms nor contradicts")
	}
}
