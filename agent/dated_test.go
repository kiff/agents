package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/tool"
)

// A vendor's times are dated beside their numbers, which stay as they came:
// by the field's name, read with its parent's, in seconds or milliseconds
// (kiff/tafs#476: "the due date on both was 1793892934").
func TestAVendorsTimesAreDatedBesideTheirNumbers(t *testing.T) {
	invoice := `{"id":"in_1","number":"FQUL8TR6-0023","due_date":1793892934,"created":1790000000,"period_start":1789000000,` +
		`"status_transitions":{"finalized_at":1790000100,"paid_at":null},"lines":{"data":[{"period":{"start":1789000000,"end":1791000000}}]},` +
		`"dueDate":1793892934,"createdAt":1790000000000,"expires":1793892934,"timestamp":1793892934}`
	want := `{"id":"in_1","number":"FQUL8TR6-0023","due_date":1793892934 (2026-11-05),"created":1790000000 (2026-09-21),"period_start":1789000000 (2026-09-10),` +
		`"status_transitions":{"finalized_at":1790000100 (2026-09-21),"paid_at":null},"lines":{"data":[{"period":{"start":1789000000 (2026-09-10),"end":1791000000 (2026-10-03)}}]},` +
		`"dueDate":1793892934 (2026-11-05),"createdAt":1790000000000 (2026-09-21),"expires":1793892934 (2026-11-05),"timestamp":1793892934 (2026-11-05)}`
	if got := dated(invoice, nil); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	// A list of them too.
	if got := dated(`[{"created":1790000000},{"created":1790000000}]`, nil); got != `[{"created":1790000000 (2026-09-21)},{"created":1790000000 (2026-09-21)}]` {
		t.Fatalf("a list: %s", got)
	}
}

// Never a number that only looks like a time: an amount, an id, a field
// whose name has "date" in its letters but not as a word, a time given as
// text, one outside 2000 to 2100, or a number in a list with no field.
func TestOnlyTimesAreDated(t *testing.T) {
	for _, text := range []string{
		`{"amount_due":1793892934,"id":1793892934,"customer":1790000000}`,
		`{"update":1793892934,"candidate":1793892934,"validated":1793892934,"metadata":{"order":1793892934}}`,
		`{"due_date":"1793892934","created_at":"2026-11-05T10:00:00Z","ts":"1700000000.000200"}`,
		`{"due_date":123,"created_at":5000000000,"expires_at":-1,"period_end":0}`,
		`{"dates":[1790000000,1793892934]}`,
		`{"at":1790000000,"period":1790000000}`,
		"Refunded ch_1 on 1790000000.", `{ "due_date": 1793892934`, "", "1793892934",
	} {
		if got := dated(text, nil); got != text {
			t.Errorf("%q dated as %q", text, got)
		}
	}
}

// The schema comes first: a field it marks as a time is dated whatever its
// name, in the unit it says; a field it doesn't mark is read by its name.
func TestTheResultSchemaComesFirst(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{` +
		`"available_on":{"type":"integer","format":"unix-time"},` +
		`"arrival":{"type":["integer","null"],"description":"Milliseconds since the Unix epoch."},` +
		`"charges":{"type":"array","items":{"type":"object","properties":{"settled":{"type":"integer","description":"Time at which it settled. Measured in seconds since the Unix epoch."}}}},` +
		`"amount":{"type":"integer","description":"An amount in cents."},` +
		`"label":{"type":"string","format":"date-time"}}}`)
	units := timeUnits(&vendorTool{gateway: true, schema: schema})
	if len(units) != 3 || units["available_on"] != time.Second || units["arrival"] != time.Millisecond || units["settled"] != time.Second {
		t.Fatalf("units %v", units)
	}
	got := dated(`{"available_on":1793892934,"arrival":1793892934000,"charges":[{"settled":1790000000}],"amount":1793892934,"due_date":1793892934}`, units)
	want := `{"available_on":1793892934 (2026-11-05),"arrival":1793892934000 (2026-11-05),"charges":[{"settled":1790000000 (2026-09-21)}],"amount":1793892934,"due_date":1793892934 (2026-11-05)}`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	// A unit the schema gives is the only one read: milliseconds aren't
	// seconds out of range.
	if got := dated(`{"arrival":1793892934}`, units); got != `{"arrival":1793892934}` {
		t.Fatalf("seconds read as the schema's milliseconds: %s", got)
	}
	if timeUnits(&vendorTool{gateway: true}) != nil || timeUnits(&vendorTool{gateway: true, schema: json.RawMessage(`{"type":"object"}`)}) != nil {
		t.Fatal("units from no schema")
	}
}

// Only a vendor's answer, through the gateway, is dated: the agent's own
// tools answer as they did.
func TestOnlyAVendorsAnswersAreDated(t *testing.T) {
	answer := `{"due_date":1793892934}`
	use := model.Block{Kind: model.ToolUse, CallID: "c1", Tool: "list_invoices", Input: json.RawMessage(`{}`)}
	if r := call(context.Background(), &vendorTool{gateway: true, result: answer}, use, DefaultMaxToolResult); r.Text != `{"due_date":1793892934 (2026-11-05)}` {
		t.Fatalf("a vendor's answer: %s", r.Text)
	}
	if r := call(context.Background(), &vendorTool{result: answer}, use, DefaultMaxToolResult); r.Text != answer {
		t.Fatalf("the agent's own tool: %s", r.Text)
	}
}

// vendorTool answers every call with result, through the gateway or not,
// with a result schema when it has one.
type vendorTool struct {
	gateway bool
	result  string
	schema  json.RawMessage
}

func (v *vendorTool) Spec() model.ToolSpec {
	return model.ToolSpec{Name: "list_invoices", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (v *vendorTool) Access() tool.Access           { return tool.Read }
func (v *vendorTool) Gateway() bool                 { return v.gateway }
func (v *vendorTool) ResultSchema() json.RawMessage { return v.schema }
func (v *vendorTool) Call(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{Text: v.result}, nil
}
