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
