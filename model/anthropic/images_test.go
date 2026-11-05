package anthropic_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/model/anthropic"
)

// screenshots is a turn whose tool results each carry one image, n in all.
func screenshots(n int) model.Request {
	var uses, results []model.Block
	for i := range n {
		id := fmt.Sprintf("toolu_%d", i)
		uses = append(uses, model.Block{Kind: model.ToolUse, CallID: id, Tool: "app_look", Input: json.RawMessage(`{}`)})
		results = append(results, model.Block{Kind: model.ToolResult, CallID: id, Result: fmt.Sprintf("look %d", i),
			Images: []model.Image{{MediaType: "image/jpeg", Bytes: []byte(fmt.Sprintf("jpeg %d", i))}}})
	}
	return model.Request{Model: "test-model", MaxTokens: 512, Messages: []model.Message{
		{Role: model.User, Blocks: []model.Block{{Kind: model.Text, Text: "Show me how."}}},
		{Role: model.Assistant, Blocks: uses},
		{Role: model.User, Blocks: results},
	}}
}

// sentResults is each tool result's content as sent, in order.
func sentResults(t *testing.T, sent map[string]any) [][]map[string]any {
	t.Helper()
	msgs := sent["messages"].([]any)
	var out [][]map[string]any
	for _, b := range msgs[len(msgs)-1].(map[string]any)["content"].([]any) {
		var content []map[string]any
		raw, _ := json.Marshal(b.(map[string]any)["content"])
		if err := json.Unmarshal(raw, &content); err != nil {
			t.Fatal(err)
		}
		out = append(out, content)
	}
	return out
}

// A tool's images (a drive's screenshot, kiff/tafs#544) go in its result,
// after its text, each as an image the model reads.
func TestAToolsImagesGoInItsResult(t *testing.T) {
	m, sent := serve(t, stream)
	if _, err := m.Turn(context.Background(), screenshots(2), nil); err != nil {
		t.Fatal(err)
	}
	got := sentResults(t, *sent)
	if len(got) != 2 || len(got[0]) != 2 || got[0][0]["text"] != "look 0" || got[0][1]["type"] != "image" {
		t.Fatalf("results %v", got)
	}
	src := got[1][1]["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/jpeg" || src["data"] != base64.StdEncoding.EncodeToString([]byte("jpeg 1")) {
		t.Fatalf("image source %v", src)
	}
}

// Past ImagesKept, the oldest images leave ImagesDropStep at a time, each a
// line saying so: the request's prefix stays the same for that many turns,
// so the cache holds, the model always reads the latest ones, and a request
// never holds more than ImagesKept.
func TestTheOldestImagesLeaveInSteps(t *testing.T) {
	images := func(got [][]map[string]any) (shown, gone int) {
		for _, r := range got {
			for _, c := range r[1:] {
				if c["type"] == "image" {
					shown++
				} else if strings.Contains(fmt.Sprint(c["text"]), "no longer in view") {
					gone++
				}
			}
		}
		return
	}
	for _, tc := range []struct{ n, gone int }{
		{anthropic.ImagesKept, 0},
		{anthropic.ImagesKept + 1, anthropic.ImagesDropStep},
		{anthropic.ImagesKept + anthropic.ImagesDropStep, anthropic.ImagesDropStep},
		{anthropic.ImagesKept + anthropic.ImagesDropStep + 1, 2 * anthropic.ImagesDropStep},
	} {
		m, sent := serve(t, stream)
		if _, err := m.Turn(context.Background(), screenshots(tc.n), nil); err != nil {
			t.Fatal(err)
		}
		got := sentResults(t, *sent)
		shown, gone := images(got)
		if gone != tc.gone || shown != tc.n-tc.gone {
			t.Errorf("%d images: %d shown, %d gone; want %d gone", tc.n, shown, gone, tc.gone)
		}
		// Never more than ImagesKept in one request: past 20, the model holds every image to a stricter size.
		if shown > anthropic.ImagesKept {
			t.Errorf("%d images: %d in one request", tc.n, shown)
		}
		// The latest is always read, the oldest first to go.
		if got[len(got)-1][1]["type"] != "image" || (tc.gone > 0 && got[0][1]["type"] == "image") {
			t.Errorf("%d images: the wrong ones went", tc.n)
		}
	}
}
