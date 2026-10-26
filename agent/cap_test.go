package agent

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kiff/agents/model"
)

func TestALongToolResultIsCutAndSaysSo(t *testing.T) {
	long := strings.Repeat("é", MaxToolResult) // 2 bytes a rune: twice the cap
	got := capResult(long, MaxToolResult)
	if !utf8.ValidString(got) {
		t.Fatal("cut inside a rune")
	}
	if !strings.HasPrefix(got, long[:MaxToolResult]) || !strings.Contains(got, "cut at 128 KB of 256 KB") || !strings.Contains(got, "Call again for less") {
		t.Fatalf("got …%q", got[len(got)-200:])
	}
	if short := "ok"; capResult(short, MaxToolResult) != short {
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
