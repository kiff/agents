package agent

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/tool"
)

func TestALongToolResultIsCutAndSaysSo(t *testing.T) {
	long := strings.Repeat("é", DefaultMaxToolResult) // 2 bytes a rune: twice the cap
	got := capResult(long, DefaultMaxToolResult)
	if !utf8.ValidString(got) {
		t.Fatal("cut inside a rune")
	}
	if !strings.HasPrefix(got, long[:DefaultMaxToolResult]) || !strings.Contains(got, "cut at 32 KB of 64 KB") || !strings.Contains(got, "Call again for less") {
		t.Fatalf("got …%q", got[len(got)-200:])
	}
	if short := "ok"; capResult(short, DefaultMaxToolResult) != short {
		t.Fatal("a short result was changed")
	}
}

// Results the model has read are cleared all at once, past the threshold,
// never the newest; under it, nothing changes, so the cache holds.
func TestReadResultsAreClearedTogether(t *testing.T) {
	big := strings.Repeat("x", 30<<10)
	use := func(id, tool string) model.Message {
		return model.Message{Role: model.Assistant, Blocks: []model.Block{{Kind: model.ToolUse, CallID: id, Tool: tool}}}
	}
	result := func(id, text string) model.Message {
		return model.Message{Role: model.User, Blocks: []model.Block{{Kind: model.ToolResult, CallID: id, Result: text}}}
	}
	msgs := []model.Message{use("a", "list_charges"), result("a", big), use("b", "list_charges"), result("b", big)}
	if got := clearRead(msgs, 48<<10); got[1].Blocks[0].Result != big || got[3].Blocks[0].Result != big {
		t.Fatal("cleared under the threshold: only one read result is 30 KB")
	}
	msgs = append(msgs, use("c", "get_charge"), result("c", "ch_1"))
	got := clearRead(msgs, 48<<10)
	if want := fmt.Sprintf(ClearedNote, "list_charges", 30); got[1].Blocks[0].Result != want || got[3].Blocks[0].Result != want {
		t.Fatalf("read results = %q, %q; want both cleared", got[1].Blocks[0].Result, got[3].Blocks[0].Result)
	}
	if got[5].Blocks[0].Result != "ch_1" {
		t.Fatal("the newest result was cleared before the model read it")
	}
	if msgs[1].Blocks[0].Result != big {
		t.Fatal("clearing changed the caller's messages")
	}
	// Once cleared, the notes don't count: nothing is cleared again until
	// more is read.
	if again := clearRead(got, 48<<10); again[5].Blocks[0].Result != "ch_1" {
		t.Fatal("cleared again with nothing new read")
	}
}

// A vendor's indented JSON reaches the model compacted, values untouched;
// anything that isn't wholly JSON stays as it is.
func TestJSONResultsAreCompacted(t *testing.T) {
	indented := "{\n  \"data\": [\n    {\"id\": \"ch_1\", \"description\": \"two  spaces\\n kept\"}\n  ],\n  \"has_more\": true\n}\n"
	if got, want := compactJSON(indented), `{"data":[{"id":"ch_1","description":"two  spaces\n kept"}],"has_more":true}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	for _, text := range []string{"Refunded ch_1.", "{ not json", "[1, 2] and more", "", "42"} {
		if got := compactJSON(text); got != text {
			t.Fatalf("%q changed to %q", text, got)
		}
	}
}

// The definition's bound applies after compaction; 0 is the default.
func TestTheDefinitionBoundsOneResult(t *testing.T) {
	if (Agent{}).maxToolResult() != DefaultMaxToolResult || (Agent{MaxToolResult: 4 << 10}).maxToolResult() != 4<<10 {
		t.Fatal("the bound isn't the definition's, or the default")
	}
	items := make([]string, 400)
	for i := range items {
		items[i] = fmt.Sprintf("{\n    \"id\": \"ch_%03d\",\n    \"amount\": 15000\n  }", i)
	}
	page := "{\n  \"data\": [\n  " + strings.Join(items, ",\n  ") + "\n  ]\n}"
	got := capResult(compactJSON(page), 4<<10)
	if !strings.HasPrefix(got, `{"data":[{"id":"ch_000","amount":15000},`) || !strings.Contains(got, "cut at 4 KB of") || !strings.Contains(got, "with a filter or a smaller limit") {
		t.Fatalf("got %q…", got[:120])
	}
}

// A tool's images (a drive's screenshot, kiff/tafs#544) reach the model with
// its result, and leave with its text when the run clears what was read: in
// the same clearing, so the cache breaks once.
func TestAToolsImagesComeAndGoWithItsResult(t *testing.T) {
	img := []model.Image{{MediaType: "image/jpeg", Bytes: []byte("jpeg")}}
	b := resultBlock(ToolResultBody{CallID: "c1", Tool: "app_look", Result: tool.Result{Text: "Looked", Images: img}})
	if len(b.Images) != 1 || b.Result != "Looked" {
		t.Fatalf("block = %+v", b)
	}
	big := strings.Repeat("x", 49<<10)
	msgs := []model.Message{
		{Role: model.Assistant, Blocks: []model.Block{{Kind: model.ToolUse, CallID: "c1", Tool: "app_look"}}},
		{Role: model.User, Blocks: []model.Block{{Kind: model.ToolResult, CallID: "c1", Result: big, Images: img}}},
		{Role: model.Assistant, Blocks: []model.Block{{Kind: model.ToolUse, CallID: "c2", Tool: "app_look"}}},
		{Role: model.User, Blocks: []model.Block{{Kind: model.ToolResult, CallID: "c2", Result: "new", Images: img}}},
	}
	got := clearRead(msgs, 48<<10)
	if got[1].Blocks[0].Images != nil || !cleared(got[1].Blocks[0].Result) {
		t.Fatalf("a cleared result kept its images: %+v", got[1].Blocks[0])
	}
	if len(got[3].Blocks[0].Images) != 1 {
		t.Fatal("the newest result lost its image")
	}
	if len(msgs[1].Blocks[0].Images) != 1 {
		t.Fatal("clearing changed the messages it was given")
	}
}
