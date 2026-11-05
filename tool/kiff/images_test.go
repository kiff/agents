package kiff_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kiff/agents/tool/kiff"
)

// A tool's images (a screenshot of the app Coach drives, kiff/tafs#544) come
// with its text for the model: an image type the model reads, within the
// bounds; anything else is a line saying so, never dropped silently. They
// never enter the result's recorded form.
func TestAToolsImagesComeWithItsText(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nnot really, but bytes")
	big := make([]byte, kiff.MaxImageBytes+1)
	content := []any{
		map[string]any{"type": "text", "text": "Tapped Run now"},
		map[string]any{"type": "image", "data": base64.StdEncoding.EncodeToString(png), "mimeType": "image/png"},
		map[string]any{"type": "image", "data": "not base64!", "mimeType": "image/png"},
		map[string]any{"type": "image", "data": base64.StdEncoding.EncodeToString(png), "mimeType": "image/tiff"},
		map[string]any{"type": "image", "data": base64.StdEncoding.EncodeToString(big), "mimeType": "image/jpeg"},
	}
	for range kiff.MaxImages {
		content = append(content, map[string]any{"type": "image", "data": base64.StdEncoding.EncodeToString(png), "mimeType": "image/jpeg"})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &msg)
		if msg.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s")
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "app_look", "inputSchema": map[string]any{"type": "object"}, "annotations": map[string]any{"readOnlyHint": true}}}}
		case "tools/call":
			result = map[string]any{"content": content}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
	}))
	defer srv.Close()
	tools, err := kiff.New(srv.URL, testKey, srv.Client()).Tools(context.Background())
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools = %v, %v", tools, err)
	}
	got, err := tools[0].Call(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Images) != kiff.MaxImages || got.Images[0].MediaType != "image/png" || string(got.Images[0].Bytes) != string(png) {
		t.Fatalf("images = %d, first %+v", len(got.Images), got.Images)
	}
	for _, line := range []string{"Tapped Run now", "[an image the tool sent that couldn't be read]", "over the 1048576 a tool's image may be", "[more than 4 images: the rest left out]"} {
		if !strings.Contains(got.Text, line) {
			t.Errorf("text %q is missing %q", got.Text, line)
		}
	}
	if strings.Count(got.Text, "couldn't be read") != 2 {
		t.Errorf("a bad image and an unread type each say so: %q", got.Text)
	}
	// The recorded form never carries them.
	rec, _ := json.Marshal(got)
	if strings.Contains(string(rec), base64.StdEncoding.EncodeToString(png)) || strings.Contains(string(rec), `"images"`) || strings.Contains(string(rec), `"Images"`) {
		t.Fatalf("recorded: %s", rec)
	}
}
